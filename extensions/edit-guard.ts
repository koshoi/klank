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
//   3. Bash interceptor    - edit-shaped shell commands (sed -i, perl -i,
//                            awk inplace, echo/printf redirects) are blocked
//                            with a message pointing at the edit/write tools.
//   4. Prompt rule         - a <critical> system-prompt bullet reinforces the
//                            tool discipline for the cases regex cannot catch.
//
// No configuration required. Block reasons always tell the model exactly how
// to recover (re-read), so a blocked call costs one retry, not a dead end.

import * as crypto from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// --- snapshot store ---------------------------------------------------------

interface Snapshot {
	hash: string; // short content fingerprint of the file as last seen
}

const snapshots = new Map<string, Snapshot>(); // key: canonical path

const hashText = (text: string) =>
	crypto.createHash("sha256").update(text.replace(/\r\n/g, "\n")).digest("hex").slice(0, 8);

const canonical = (p: string): string => {
	try {
		return fs.realpathSync.native(p);
	} catch {
		return path.resolve(p);
	}
};

/** Hash the file as it is on disk right now; null for unreadable/binary. */
const currentSnapshot = (p: string): Snapshot | null => {
	try {
		const st = fs.statSync(p);
		if (!st.isFile()) return null;
		const text = fs.readFileSync(p, "utf8");
		return { hash: hashText(text) };
	} catch {
		return null; // binary (utf8 replacement chars are fine) or vanished
	}
};

const remember = (p: string) => {
	const snap = currentSnapshot(p);
	if (snap) snapshots.set(canonical(p), snap);
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
	const promptRule =
		"<critical>NEVER use `sed`|`perl`|`python` via bash to issue individual edits; " +
		"use the `edit` tool. Files must be `read` before `edit`/`write`; if a guard " +
		"message says the file changed, re-read it first.</critical>";

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
				return {
					block: true,
					reason:
						`File changed since you last read it (snapshot ${seen.hash}, on disk ${now.hash}). ` +
						"Something (bash, eval, another process) modified it out-of-band. " +
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
			const files = [...snapshots.keys()].map((k) => k.replace(ctx.cwd, "."));
			ctx.ui.notify(
				`edit-guard: ${files.length} tracked snapshot(s)\n${files.join("\n")}`,
				"info",
			);
		},
	});
}
