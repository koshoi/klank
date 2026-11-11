// edit-guard.ts - pi extension: snapshot-based edit guardrails.
//
// Three layers, modeled on omp's hashline editing + bash intent interceptor:
//
//   1. Snapshot tracking   - every successful `read` records a content hash of
//                            the file as the model last saw it; successful
//                            `edit`/`write` refresh the snapshot.
//   2. Freshness enforcing - `edit`/`write` are blocked when the file changed
//                            since the last read (hash mismatch), which catches
//                            out-of-band mutations (bash, eval, manual edits)
//                            before they corrupt the model's mental model.
//                            When the staleness is locally explainable, the
//                            block message includes a partial-staleness
//                            reveal (a line diff of the changed regions,
//                            like omp's stale-tag reveal) and refreshes the
//                            snapshot, so the model can fix up its edit from
//                            the message alone instead of re-reading the
//                            whole file.
//   3. Bash interceptor    - edit-shaped shell commands (sed -i, perl -i,
//                            awk inplace, echo/printf redirects) are blocked
//                            with a message pointing at the edit/write tools.
//   4. Prompt rule         - a <critical> system-prompt bullet reinforces the
//                            tool discipline for the cases regex cannot catch.
//
// No configuration required. Block reasons always tell the model exactly how
// to recover (use the shown diff, or re-read), so a blocked call costs one
// retry, not a dead end.

import * as crypto from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { fileHash } from "./hashline.ts";

// --- snapshot store ---------------------------------------------------------

// Content is kept so a staleness block can show a diff; very large files
// store hash only (reveal is not worth holding megabytes per file).
const CONTENT_CAP = 512 * 1024;

interface Snapshot {
	hash: string; // short content fingerprint of the file as last seen
	tag?: string; // 4-hex display tag (the [path#TAG] from read output); prefix of `hash`
	text?: string; // content as last seen (omitted when too large)
}

const snapshots = new Map<string, Snapshot>(); // key: canonical path

// Same normalization as hashline.ts's fileHash (BOM strip + CRLF→LF) so the
// 4-hex read tag is always a strict prefix of the 8-hex snapshot hash.
const hashText = (text: string) =>
	crypto
		.createHash("sha256")
		.update(text.replace(/^\uFEFF/, "").replace(/\r\n/g, "\n"))
		.digest("hex")
		.slice(0, 8)
		.toUpperCase();

const canonical = (p: string): string => {
	try {
		return fs.realpathSync.native(p);
	} catch {
		return path.resolve(p);
	}
};

/** Snapshot the file as it is on disk right now; null for unreadable/binary. */
const currentSnapshot = (p: string): Snapshot | null => {
	try {
		const st = fs.statSync(p);
		if (!st.isFile()) return null;
		const text = fs.readFileSync(p, "utf8");
		return {
			hash: hashText(text),
			tag: fileHash(text),
			text: text.length <= CONTENT_CAP ? text : undefined,
		};
	} catch {
		return null; // binary (utf8 replacement chars are fine) or vanished
	}
};

const remember = (p: string) => {
	const snap = currentSnapshot(p);
	if (snap) snapshots.set(canonical(p), snap);
};

// --- line diff (LCS) ---------------------------------------------------------

interface Hunk {
	oldStart: number; // 1-based line number in the file as last seen
	newStart: number; // 1-based line number in the file as it is now
	lines: string[]; // rendered "- old" / "+ new" rows
}

const MAX_DIFF_CELLS = 2_000_000; // LCS table budget (oldTail * newTail)
const MAX_REVEAL_LINES = 80; // rendered diff rows per block message

/** Unified-style hunk headers: @@ -a,b +c,d @@ (ranges elided when length 1). */
const hunkHeader = (h: Hunk): string => {
	const oldLen = h.lines.filter((l) => l.startsWith("-")).length;
	const newLen = h.lines.filter((l) => l.startsWith("+")).length;
	const range = (n: number, len: number) => (len === 1 ? `${n}` : `${n},${len}`);
	return `@@ -${range(h.oldStart, oldLen)} +${range(h.newStart, newLen)} @@`;
};

/**
 * Line-level diff of two texts, grouped into changed hunks. Returns null when
 * the change is too large to align within budget — the caller then falls back
 * to the plain re-read message instead of a misleading partial reveal.
 */
const diffHunks = (oldText: string, newText: string): Hunk[] | null => {
	const a = oldText.split("\n");
	const b = newText.split("\n");

	// trim common prefix/suffix so the LCS only covers the changed core
	let start = 0;
	while (start < a.length && start < b.length && a[start] === b[start]) start++;
	let endA = a.length;
	let endB = b.length;
	while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
		endA--;
		endB--;
	}
	const oldTail = a.slice(start, endA);
	const newTail = b.slice(start, endB);
	if (oldTail.length === 0 && newTail.length === 0) return []; // hash said otherwise (CRLF?) — no reveal
	if (oldTail.length * newTail.length > MAX_DIFF_CELLS) return null;

	// LCS table over the changed core
	const n = oldTail.length;
	const m = newTail.length;
	const table = new Int32Array((n + 1) * (m + 1));
	const at = (i: number, j: number) => i * (m + 1) + j;
	for (let i = n - 1; i >= 0; i--) {
		for (let j = m - 1; j >= 0; j--) {
			table[at(i, j)] =
				oldTail[i] === newTail[j]
					? table[at(i + 1, j + 1)] + 1
					: Math.max(table[at(i + 1, j)], table[at(i, j + 1)]);
		}
	}

	// walk the table, grouping del/ins runs into hunks
	const hunks: Hunk[] = [];
	let i = 0;
	let j = 0;
	let current: Hunk | null = null;
	const push = (row: string) => {
		if (!current) {
			current = {
				oldStart: start + i + 1,
				newStart: start + j + 1,
				lines: [],
			};
			hunks.push(current);
		}
		current.lines.push(row);
	};
	const close = () => {
		current = null;
	};
	while (i < n && j < m) {
		if (oldTail[i] === newTail[j]) {
			close();
			i++;
			j++;
		} else if (table[at(i + 1, j)] >= table[at(i, j + 1)]) {
			push(`- ${oldTail[i]}`);
			i++;
		} else {
			push(`+ ${newTail[j]}`);
			j++;
		}
	}
	while (i < n) push(`- ${oldTail[i++]}`);
	close();
	while (j < m) push(`+ ${newTail[j++]}`);
	close();
	return hunks;
};

/** Render hunks into the reveal block; null when it would exceed the cap. */
const renderReveal = (hunks: Hunk[]): string | null => {
	const out: string[] = [];
	for (const h of hunks) {
		const header = hunkHeader(h);
		if (out.length + h.lines.length + 1 > MAX_REVEAL_LINES) return null;
		out.push(header, ...h.lines);
	}
	return out.join("\n");
};

// --- bash intent interceptor ------------------------------------------------

interface BashRule {
	pattern: RegExp;
	tool: string;
	message: string;
}

const BASH_RULES: BashRule[] = [
	{
		pattern: /^\s*sed\s+(-[a-zA-Z]*i|--in-place)/,
		tool: "edit",
		message: "Blocked: `sed -i`. Use the `edit` tool instead — it validates against a snapshot of the file and previews the change.",
	},
	{
		pattern: /^\s*perl\s+[^|;&]*-([a-zA-Z]*i)/,
		tool: "edit",
		message: "Blocked: `perl -i`. Use the `edit` tool instead.",
	},
	{
		pattern: /^\s*awk\s+[^|;&]*-i\s+inplace/,
		tool: "edit",
		message: "Blocked: `awk -i inplace`. Use the `edit` tool instead.",
	},
	{
		// echo/printf whose stdout lands in a real file (not /dev/null, not a pipe)
		pattern: /^\s*(?:echo|printf)\s+[^|;&]*?>{1,2}\s*(?![|\n])((?!\/dev\/)[\w./~"'${}-]+)/,
		tool: "write",
		message: "Blocked: shell output redirection to a file. Use the `write` tool instead (it replaces the whole file; for appends use `edit`).",
	},
	{
		pattern: /^\s*cat\s*<<[^|;&]*?>{1,2}\s*[\w./~"'-]+/,
		tool: "write",
		message: "Blocked: heredoc redirection. Use the `write` tool instead.",
	},
];

const interceptBash = (command: string, hasTool: (name: string) => boolean): string | undefined => {
	// mask quoted strings and command substitutions so quotes/paths inside them
	// (e.g. an arrow in "a -> b", a `;` in "$(cmd)") can't create fake operators;
	// then check each command segment separately so `cd x && sed -i ...` and
	// pipes still match rules anchored at the command position
	const masked = command
		.replace(/`[^`]*`/g, "`´`")
		.replace(/\$\([^()]*\)/g, "$()")
		.replace(/"[^"\n]*"/g, '""')
		.replace(/'[^'\n]*'/g, "''");
	for (const segment of masked.split(/&&|;|\|/)) {
		for (const rule of BASH_RULES) {
			// only redirect when the suggested tool is actually available this session
			if (rule.pattern.test(segment) && hasTool(rule.tool)) {
				return rule.message;
			}
		}
	}
	return undefined;
};

// --- extension --------------------------------------------------------------

export default function (pi: ExtensionAPI) {
	const tagOf = (s: Snapshot) => s.tag ?? s.hash.slice(0, 4);

	const promptRule =
		"<critical>NEVER use `sed`|`perl`|`python` via bash to issue individual edits; " +
		"use the `edit` tool. Files must be `read` before `edit`/`write`; if a guard " +
		"message says the file changed, use the shown region diff to fix up your " +
		"change and retry — re-read (offset/limit) only when you need more context. " +
		"read output starts with [path#TAG] — quote the tag when reporting file state; " +
		"line numbers are informational, edit with exact oldText only — content after the " +
		"N: prefix is verbatim file text including indentation; reproduce tabs/spaces " +
		"byte-for-byte.</critical>";

	// Layer 4: tool-discipline rule in the system prompt
	pi.on("before_agent_start", async (event) => {
		const guidelines = event.systemPromptOptions.promptGuidelines;
		if (Array.isArray(guidelines) && !guidelines.includes(promptRule)) {
			guidelines.push(promptRule);
		}
	});

	// Layer 1: snapshot on every successful read; refresh after edit/write
	pi.on("tool_result", async (event) => {
		if (event.isError) return;
		const input = (event.input ?? {}) as { path?: string; file_path?: string };
		const p = input.path ?? input.file_path;
		if (typeof p === "string" && ["read", "edit", "write"].includes(event.toolName)) {
			remember(p);
		}
	});

	// Layers 2 + 3: enforce freshness and shell discipline before execution
	pi.on("tool_call", async (event) => {
		// Layer 3: bash intent interceptor
		if (event.toolName === "bash") {
			const input = event.input as { command?: string };
			const command = input?.command ?? "";
			const message = interceptBash(command, (name) => {
				try {
					return pi.getAllTools().some((t) => t.name === name);
				} catch {
					return true;
				}
			});
			if (message) return { block: true, reason: message };
			return;
		}

		// Layer 2: freshness enforcement for edit/write
		if (event.toolName === "edit" || event.toolName === "write") {
			const input = (event.input ?? {}) as { path?: string; file_path?: string };
			const p = input.path ?? input.file_path;
			if (typeof p !== "string" || !p) return;

			const key = canonical(p);
			const seen = snapshots.get(key);
			const now = currentSnapshot(p);

			// file vanished since read -> let the tool produce its own error
			if (!now) return;

			if (seen && seen.hash !== now.hash) {
				// Partial-staleness reveal: when we still have what the model saw
				// and the change is small enough to align, show the changed
				// regions (with current line numbers) and refresh the snapshot so
				// the model can retry immediately — no full re-read needed.
				const reveal =
					seen.text !== undefined && now.text !== undefined
						? renderReveal(diffHunks(seen.text, now.text) ?? [])
						: null;
				if (reveal) {
					snapshots.set(key, now); // revealed state is now the model's baseline
					return {
						block: true,
						reason:
							`File changed since you last read it (snapshot ${tagOf(seen)}, on disk now ${tagOf(now)}). ` +
							"Something (bash, eval, another process) modified it out-of-band. " +
							"Changed regions (old line numbers → current):\n\n" +
							reveal +
							"\n\n" +
							"edit-guard: snapshot refreshed — apply your change against the current content shown above and retry. " +
							"Use `read` with offset/limit for a specific region only if you need more context.",
					};
				}
				return {
					block: true,
					reason:
						`File changed since you last read it (snapshot ${tagOf(seen)}, on disk now ${tagOf(now)}). ` +
						"Something (bash, eval, another process) modified it out-of-band, and the change is too large to summarize. " +
						"Re-read the file and re-apply your change against the current content.",
				};
			}

			// successful write replaces the whole file without a prior read:
			// allow it, the tool_result handler records the new snapshot
		}
	});

	// debug surface: /edit-guard shows tracked snapshots
	pi.registerCommand("edit-guard", {
		description: "Show edit-guard snapshot stats",
		handler: async (_args, ctx) => {
			const entries = [...snapshots.entries()];
			const withText = entries.filter(([, s]) => s.text !== undefined).length;
			const list = entries.map(([k]) => k.replace(ctx.cwd, ".")).join("\n");
			ctx.ui.notify(
				`edit-guard: ${entries.length} tracked snapshot(s) (${withText} with reveal content)\n${list}`,
				"info",
			);
		},
	});
}
