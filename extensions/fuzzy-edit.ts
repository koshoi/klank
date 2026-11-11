// fuzzy-edit.ts - pi extension: whitespace-insensitive fallback for `edit`.
//
// pi's built-in edit already fuzzy-matches trailing whitespace, smart quotes,
// and unicode spaces (edit-diff.js `normalizeForFuzzyMatch`) — but NOT leading
// indentation: an oldText written with spaces against a tab-indented file (or
// at a shifted nesting level) still fails with the generic "must match exactly
// including all whitespace" error. This is the #1 real-world edit failure for
// LLMs, which transcribe tabs as spaces.
//
// Modeled on omp (oh-my-pi) crates/pi-edit: the per-line "Trim" match strategy
// (fuzzy.rs run_sequence_passes) and adjust_indentation (text.rs, which
// re-indents newText to the file's indentation style after a fuzzy hit).
//
// The builtin runs first and stays authoritative; this fallback engages ONLY
// when the builtin reported "Could not find ..." (no bytes were written):
//
//   1. Whitespace-insensitive match: a block matches when every line compares
//      equal after trimming leading/trailing whitespace (tabs vs spaces vs
//      shifted nesting all disappear). The match must be UNIQUE — ambiguity
//      is an error, not a guess.
//   2. Re-indentation: newText is re-indented to the matched file region's
//      style (tab<->space conversion via indent unit, uniform nesting delta)
//      before splicing, so the file keeps a consistent style.
//   3. Whitespace-specific errors: when even the fallback cannot locate a
//      block, the error shows the closest file region with whitespace made
//      visible (tab -> ->, trailing space -> ·) and names the file's
//      indentation style vs the model's, so the next attempt is informed.
//
// edit-guard.ts imports resolveEditSpans() from here to locate edit spans for
// its seen-lines enforcement, and the seen-lines bookkeeping adjusts for the
// splices this fallback performs (same tool name "edit", so edit-guard's
// tool_result snapshot refresh applies unchanged).

import * as fs from "node:fs";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { createEditToolDefinition } from "@earendil-works/pi-coding-agent";

// --- pure text helpers (exported for tests + edit-guard) --------------------

/** Trim leading+trailing whitespace (tabs, spaces, CRLF remnants) off a line. */
const trimLine = (line: string): string => line.replace(/^[\t ]+/, "").replace(/[\t ]+$/, "");

export interface IndentProfile {
	char: " " | "\t" | null; // first indentation character found
	tabOnly: boolean;
	spaceOnly: boolean;
	mixed: boolean;
	unit: number; // GCD of space-indent widths (spaces), 1 for tab-only, 0 if none
	nonEmpty: number;
}

const gcd = (a: number, b: number): number => (b === 0 ? a : gcd(b, a % b));

/** True when any non-empty line carries leading whitespace. A block with NO
 *  indentation proves nothing about style — its spaceOnly/tabOnly flags are
 *  vacuous, and conversion must not fire on them. */
const hasIndent = (text: string): boolean =>
	text
		.split("\n")
		.some((line) => line.trim().length > 0 && /^\t| {2}/.test(line));

/** Indentation statistics of a text block (port of omp text.rs build_indent_profile). */
export const buildIndentProfile = (text: string): IndentProfile => {
	let char: " " | "\t" | null = null;
	let tabOnly = true;
	let spaceOnly = true;
	let mixed = false;
	let unit = 0;
	let nonEmpty = 0;
	for (const line of text.split("\n")) {
		if (line.trim().length === 0) continue;
		nonEmpty++;
		const indent = line.match(/^[\t ]*/)?.[0] ?? "";
		const hasSpace = indent.includes(" ");
		const hasTab = indent.includes("\t");
		if (hasSpace) tabOnly = false;
		if (hasTab) spaceOnly = false;
		if (hasSpace && hasTab) mixed = true;
		const first = indent[0] as " " | "\t" | undefined;
		if (first) {
			if (char === null) char = first;
			else if (char !== first) mixed = true;
		}
		if (indent.length > 0 && !hasTab) unit = unit === 0 ? indent.length : gcd(unit, indent.length);
	}
	if (tabOnly && nonEmpty > 0) unit = 1;
	return { char, tabOnly, spaceOnly, mixed, unit, nonEmpty };
};

/** Human name for an indent profile, used in error/result messages. */
export const describeIndent = (p: IndentProfile): string => {
	if (p.nonEmpty === 0) return "no indentation";
	if (p.mixed) return "mixed tabs+spaces";
	if (p.tabOnly) return "tabs";
	if (p.unit > 0) return `spaces (${p.unit} per level)`;
	return "spaces";
};

/**
 * Start line indices (0-based) where `patternLines` matches `lines` under the
 * per-line Trim strategy (leading/trailing whitespace ignored). Omp fuzzy.rs
 * SequenceMatchStrategy::Trim, confidence 0.98.
 */
export const trimMatchPositions = (lines: string[], patternLines: string[]): number[] => {
	const trimmed = lines.map(trimLine);
	const pattern = patternLines.map(trimLine);
	const out: number[] = [];
	if (pattern.length === 0 || pattern.length > lines.length) return out;
	outer: for (let i = 0; i + pattern.length <= lines.length; i++) {
		for (let j = 0; j < pattern.length; j++) {
			if (trimmed[i + j] !== pattern[j]) continue outer;
		}
		out.push(i);
	}
	return out;
};

/** Make whitespace visible: tab -> `->`, trailing space -> `·`. */
export const visibleWhitespace = (line: string): string =>
	line.replace(/\t/g, "->").replace(/([\t ]+)$/, (m) => "·".repeat(m.length));

/**
 * Re-indent `newText` to match the indentation of the actual file region that
 * the (whitespace-insensitively matched) oldText landed on. Port of omp's
 * adjust_indentation (text.rs), tab<->space conversion included. Returns the
 * text plus an optional human note when a conversion was applied.
 */
export const adjustNewText = (
	oldText: string,
	actualText: string,
	newText: string,
): { text: string; note?: string } => {
	if (oldText === actualText || newText.length === 0) return { text: newText };
	const oldP = buildIndentProfile(oldText);
	const actP = buildIndentProfile(actualText);
	const newP = buildIndentProfile(newText);
	if (oldP.nonEmpty === 0 || actP.nonEmpty === 0 || newP.nonEmpty === 0) return { text: newText };
	if (oldP.mixed || actP.mixed || newP.mixed) return { text: newText };

	// Indent-char conversion: model demonstrably wrote the other style than
	// the file uses (both sides must actually carry indentation evidence).
	if (oldP.tabOnly && actP.spaceOnly && actP.unit > 0 && hasIndent(oldText) && hasIndent(newText)) {
		const converted = newText
			.split("\n")
			.map((line) => {
				const indent = line.match(/^\t*/)?.[0] ?? "";
				return indent.length === 0 ? line : " ".repeat(indent.length * actP.unit) + line.slice(indent.length);
			})
			.join("\n");
		return {
			text: converted,
			note: converted !== newText ? `file indents with ${describeIndent(actP)}; your tab indentation was converted` : undefined,
		};
	}
	if (oldP.spaceOnly && actP.tabOnly && hasIndent(oldText) && hasIndent(newText) && hasIndent(actualText)) {
		const fromUnit = oldP.unit > 0 ? oldP.unit : 4;
		const converted = newText
			.split("\n")
			.map((line) => {
				const indent = line.match(/^ */)?.[0] ?? "";
				if (indent.length === 0) return line;
				return "\t".repeat(Math.round(indent.length / fromUnit)) + line.slice(indent.length);
			})
			.join("\n");
		return {
			text: converted,
			note: converted !== newText ? `file indents with tabs; your space indentation was converted` : undefined,
		};
	}

	// Same indent style but shifted nesting: apply the uniform delta between
	// the authored oldText and the matched actual region (omp compute_uniform_indent_delta).
	const lead = (line: string) => (line.match(/^[\t ]*/)?.[0] ?? "").length;
	const oldLines = oldText.split("\n");
	const actLines = actualText.split("\n");
	let delta: number | null = null;
	for (let i = 0; i < oldLines.length; i++) {
		if (trimLine(oldLines[i]).length === 0) continue;
		const d = lead(actLines[i]) - lead(oldLines[i]);
		if (delta === null) delta = d;
		else if (delta !== d) return { text: newText }; // inconsistent — leave alone
	}
	if (delta === null || delta === 0) return { text: newText };
	// omp's `new.char != actual.char` guard: newText in a different style than
	// the file would get mangled by a blind shift — leave it alone
	if (newP.char && actP.char && newP.char !== actP.char) return { text: newText };
	const indentChar = actP.char ?? oldP.char ?? " ";
	const shifted = newText
		.split("\n")
		.map((line) => {
			if (trimLine(line).length === 0) return line;
			if (delta! > 0) return indentChar.repeat(delta) + line;
			const remove = Math.min(-delta!, lead(line));
			return line.slice(remove);
		})
		.join("\n");
	return {
		text: shifted,
		note: `re-indented by ${delta > 0 ? "+" : ""}${delta} chars to match the file's nesting`,
	};
};

// --- edit-span resolution (shared with edit-guard's seen-lines check) -------

export interface EditSpan {
	editIndex: number;
	startLine: number; // 0-based, inclusive
	endLine: number; // 0-based, exclusive
	text: string; // the actual matched region
}

/**
 * Locate every edit's oldText in `content`: exact match first, then the
 * whitespace-insensitive Trim match. Null when any block cannot be located
 * uniquely (caller decides whether that is fatal).
 */
export const resolveEditSpans = (
	content: string,
	edits: ReadonlyArray<{ oldText: string; newText: string }>,
): EditSpan[] | null => {
	const lines = content.split("\n");
	const spans: EditSpan[] = [];
	for (let i = 0; i < edits.length; i++) {
		const old = edits[i].oldText;
		if (old.length === 0) return null;
		// exact match, but ONLY if unique (the builtin's contract; a duplicated
		// oldText is an ambiguity error, never "first occurrence wins")
		let first = -1;
		let count = 0;
		for (let at = content.indexOf(old); at !== -1; at = content.indexOf(old, at + 1)) {
			if (count === 0) first = at;
			count++;
		}
		if (count === 1) {
			const startLine = content.slice(0, first).split("\n").length - 1;
			const endLine = startLine + old.split("\n").length;
			spans.push({ editIndex: i, startLine, endLine, text: content.slice(first, first + old.length) });
			continue;
		}
		const pattern = old.split("\n");
		const hits = trimMatchPositions(lines, pattern);
		if (hits.length !== 1) return null; // not found, or ambiguous: no span
		spans.push({
			editIndex: i,
			startLine: hits[0],
			endLine: hits[0] + pattern.length,
			text: lines.slice(hits[0], hits[0] + pattern.length).join("\n"),
		});
	}
	return spans;
};

// --- whitespace-specific error rendering ------------------------------------

const MAX_ERROR_LINES = 8;

/** Render a line pair with whitespace made visible, aligned and numbered. */
const showPair = (label: string, lineNo: number, line: string): string =>
	`  ${label} ${String(lineNo).padStart(4)}: ${visibleWhitespace(line)}`;

/**
 * Build the whitespace-specific not-found error: names both indentation
 * styles and shows the closest file region vs the model's oldText with
 * whitespace visible, so the model knows exactly what to fix.
 */
const whitespaceError = (
	path: string,
	editIndex: number,
	total: number,
	oldText: string,
	content: string,
): Error => {
	const oldLines = oldText.split("\n");
	const fileLines = content.split("\n");
	const oldP = buildIndentProfile(oldText);
	const fileP = buildIndentProfile(content);

	// Closest region by fraction of trim-equal lines in a sliding window.
	const trimmedFile = fileLines.map(trimLine);
	const trimmedOld = oldLines.map(trimLine);
	let best = { score: 0, start: 0 };
	if (trimmedOld.length <= fileLines.length) {
		for (let i = 0; i + trimmedOld.length <= fileLines.length; i++) {
			let equal = 0;
			for (let j = 0; j < trimmedOld.length; j++) if (trimmedFile[i + j] === trimmedOld[j]) equal++;
			const score = equal / trimmedOld.length;
			if (score > best.score) best = { score, start: i };
		}
	}

	const scope = total === 1 ? "The edit" : `edits[${editIndex}]`;
	const header = `Could not find ${scope} in ${path}, even ignoring leading/trailing whitespace.`;
	const indent =
		`Indentation: the file uses ${describeIndent(fileP)}; your oldText uses ${describeIndent(oldP)}.`;
	if (best.score === 0) {
		return new Error(`${header}\n${indent}\nNo similar region exists in the file — re-read the target area and re-derive oldText from its exact current content.`);
	}
	const shown = oldLines.slice(0, MAX_ERROR_LINES).map((line, j) =>
		[
			showPair("file:", best.start + j + 1, fileLines[best.start + j] ?? ""),
			showPair("yours:", j + 1, line),
		].join("\n"),
	);
	const more = oldLines.length > MAX_ERROR_LINES ? `\n  ... (${oldLines.length - MAX_ERROR_LINES} more line pairs elided)` : "";
	const verdict =
		best.score >= 0.7
			? `Closest region (line ${best.start + 1}, ${Math.round(best.score * 100)}% of lines match up to whitespace):`
			: `Closest region (line ${best.start + 1}) is only ${Math.round(best.score * 100)}% similar up to whitespace — the content differs too. Re-read that area and re-derive oldText from its exact current content.`;
	return new Error(
		`${header}\n${indent}\n${verdict}\n${shown.join("\n")}${more}\n` +
			`Fix: reproduce the file's whitespace exactly in oldText (read the region with offset=${best.start + 1} if unsure), then retry. ` +
			`(In the pairs above, -> marks a tab and · marks a trailing space.)`,
	);
};

// --- the tool override --------------------------------------------------------

const DESCRIPTION_EXTENSION =
	" Whitespace-insensitive fallback: when oldText differs only in leading/trailing whitespace (e.g. tabs vs spaces), the edit is matched uniquely and newText is re-indented to the file's style; the success notice says so. Not-found errors show the closest region with whitespace made visible.";

interface Replacement {
	start: number; // byte offset into lf content
	end: number;
	text: string;
}

export default function (pi: ExtensionAPI) {
	const builtin = createEditToolDefinition(process.cwd());

	pi.registerTool({
		...builtin,
		name: "edit",
		label: "edit",
		description: `${builtin.description}${DESCRIPTION_EXTENSION}`,
		async execute(toolCallId, params, signal, onUpdate, ctx) {
			try {
				return await builtin.execute(toolCallId, params, signal, onUpdate, ctx);
			} catch (err) {
				const message = err instanceof Error ? err.message : String(err);
				if (!/Could not find/.test(message)) throw err;
				// Builtin failed with not-found and wrote nothing. Try the
				// whitespace-insensitive fallback; it throws a whitespace-specific
				// error when it cannot resolve every block uniquely.
				return whitespaceFallback(params, ctx?.cwd ?? process.cwd(), message);
			}
		},
	});
}

export const whitespaceFallback = (
	input: { path?: string; edits?: Array<{ oldText: string; newText: string }> },
	cwd: string,
	builtinError: string,
): { content: Array<{ type: "text"; text: string }>; details: { diff: string; patch: string; firstChangedLine?: number } } => {
	const p = input?.path;
	const edits = input?.edits;
	if (typeof p !== "string" || !Array.isArray(edits) || edits.length === 0) {
		throw new Error(builtinError);
	}
	const absolutePath = path.isAbsolute(p) ? p : path.resolve(cwd, p);
	if (!fs.existsSync(absolutePath) || !fs.statSync(absolutePath).isFile()) throw new Error(builtinError);

	const raw = fs.readFileSync(absolutePath, "utf8");
	const hadBom = raw.charCodeAt(0) === 0xfeff;
	const hadCrlf = raw.includes("\r\n");
	const bom = hadBom ? "\uFEFF" : "";
	const content = (hadBom ? raw.slice(1) : raw).replace(/\r\n/g, "\n");

	// Resolve every block: exact first, then Trim match; all must be unique.
	const lines = content.split("\n");
	const replacements: Replacement[] = [];
	const notes: string[] = [];
	for (let i = 0; i < edits.length; i++) {
		const { oldText, newText } = edits[i];
		if (oldText.length === 0) throw new Error(builtinError); // defensive: builtin rejects these first
		const pattern = oldText.split("\n");
		let startLine = -1;
		// exact match, but ONLY if unique — first-occurrence-wins would silently
		// edit the wrong place (the builtin treats duplicates as an error too)
		let exactCount = 0;
		for (let at = content.indexOf(oldText); at !== -1; at = content.indexOf(oldText, at + 1)) exactCount++;
		if (exactCount === 1) {
			startLine = content.slice(0, content.indexOf(oldText)).split("\n").length - 1;
		} else if (exactCount > 1) {
			throw new Error(
				`edits[${i}] matches ${exactCount} times in ${p}. oldText must be unique — include more surrounding lines.`,
			);
		} else {
			const hits = trimMatchPositions(lines, pattern);
			if (hits.length > 1) {
				throw new Error(
					`Whitespace-insensitive match for edits[${i}] in ${p} is ambiguous: ${hits.length} regions match up to whitespace (first at lines ${hits
						.slice(0, 5)
						.map((h) => h + 1)
						.join(", ")}). Include more surrounding lines in oldText to make it unique.`,
				);
			}
			startLine = hits[0] ?? -1;
		}
		if (startLine === -1) throw whitespaceError(p, i, edits.length, oldText, content);

		const actual = lines.slice(startLine, startLine + pattern.length).join("\n");
		const adjusted = adjustNewText(oldText, actual, newText);
		if (adjusted.note) notes.push(`edits[${i}]: ${adjusted.note}.`);
		const start = lines.slice(0, startLine).join("\n").length + (startLine > 0 ? 1 : 0);
		replacements.push({ start, end: start + actual.length, text: adjusted.text });
	}

	// Overlap check (sorted by position, like the builtin).
	replacements.sort((a, b) => a.start - b.start);
	for (let i = 1; i < replacements.length; i++) {
		if (replacements[i - 1].end > replacements[i].start) {
			throw new Error(
				`edits overlap in ${p} after whitespace-insensitive matching. Merge them into one edit or target disjoint regions.`,
			);
		}
	}

	// Splice bottom-up.
	let newContent = content;
	for (let i = replacements.length - 1; i >= 0; i--) {
		const r = replacements[i];
		newContent = newContent.slice(0, r.start) + r.text + newContent.slice(r.end);
	}
	if (newContent === content) throw new Error(builtinError);

	const finalContent = bom + (hadCrlf ? newContent.replace(/\n/g, "\r\n") : newContent);
	fs.writeFileSync(absolutePath, finalContent, "utf8");

	const details = simpleDiff(content, newContent);
	const note = notes.length > 0 ? ` ${notes.join(" ")}` : "";
	return {
		content: [
			{
				type: "text",
				text: `Successfully replaced ${edits.length} block(s) in ${p} via whitespace-insensitive matching.${note}`,
			},
		],
		details,
	};
};

/** Minimal line diff in the renderer's "+N text" format (no deep import available). */
const simpleDiff = (base: string, next: string): { diff: string; patch: string; firstChangedLine?: number } => {
	const a = base.split("\n");
	const b = next.split("\n");
	// common prefix/suffix trim, then LCS-free pairwise walk (fallback edits are small)
	let start = 0;
	while (start < a.length && start < b.length && a[start] === b[start]) start++;
	let endA = a.length;
	let endB = b.length;
	while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
		endA--;
		endB--;
	}
	const width = String(Math.max(a.length, b.length)).length;
	const pad = (n: number) => String(n).padStart(width, " ");
	const rows: string[] = [];
	for (let i = start; i < endA; i++) rows.push(`-${pad(i + 1)} ${a[i]}`);
	for (let j = start; j < endB; j++) rows.push(`+${pad(j + 1)} ${b[j]}`);
	return { diff: rows.join("\n"), patch: "", firstChangedLine: start + 1 };
};
