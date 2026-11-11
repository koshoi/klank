/**
 * hashline-read.ts - pi extension: overrides the built-in `read` tool with an
 * omp hashline-style output format.
 *
 * Text output gains a content-fingerprint header and numbered lines:
 *
 *   [src/foo.ts#A1B2]
 *   1:import ... {StringEnum} from "@earendil-works/pi-ai";
 *   2:import { Type } from "typebox";
 *   ...
 *   [Showing lines 1-120 of 340. Use offset=121 to continue.]
 *
 * The 4-hex TAG fingerprints the file as read (shared with edit-guard's
 * snapshot store, so reads, guard messages, and snapshots all reference one
 * fingerprint) and should be quoted when reporting file state. Line numbers
 * are 1-based source line numbers, stable across continuation reads, and are
 * informational only — the edit tool still requires exact oldText matching.
 *
 * Images keep the built-in read behavior entirely (the built-in definition's
 * execute is reused, not reimplemented).
 */

import * as fs from "node:fs/promises";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import {
	createReadToolDefinition,
	DEFAULT_MAX_BYTES,
	detectSupportedImageMimeTypeFromFile,
	formatSize,
	truncateHead,
	type ExtensionAPI,
} from "@earendil-works/pi-coding-agent";
import { displayPath, fileHash, formatHeader, formatNumberedLines } from "./hashline.ts";

// Mirrors pi's resolveToCwd (dist/utils/paths.js resolvePath with
// normalizeUnicodeSpaces + stripAtPrefix), which is NOT re-exported through
// the package's public exports map (only "." and "./rpc-entry"), so a deep
// import is not possible from an extension. Kept in sync with the built-in
// read's path handling: unicode-space collapse, @-prefix strip, tilde and
// file:// expansion. Omits the macOS-only NFD/screenshot/curly-quote
// fallbacks (Linux container); re-sync if pi changes path semantics.
const UNICODE_SPACES = /[\u00A0\u2000-\u200A\u202F\u205F\u3000]/g;

export const resolveReadPath = (filePath: string, cwd: string): string => {
	let normalized = filePath.trim().replace(UNICODE_SPACES, " ");
	if (normalized.startsWith("@")) normalized = normalized.slice(1);
	const home = process.env.HOME ?? "";
	if (normalized === "~" && home) normalized = home;
	else if ((normalized === "~/" || normalized.startsWith("~/")) && home) normalized = path.join(home, normalized.slice(2));
	if (/^file:\/\//.test(normalized)) normalized = fileURLToPath(normalized);
	return path.isAbsolute(normalized) ? path.normalize(normalized) : path.resolve(cwd, normalized);
};

/** Displayed/hashed text starts past the BOM, so BOM'd files show tag-matching content. */
const BOM_STRIP = (s: string): string => (s.charCodeAt(0) === 0xfeff ? s.slice(1) : s);

const DESCRIPTION_EXTENSION =
	" Output format: `[path#TAG]` header (TAG is a 4-hex fingerprint of the file as read — quote it when reporting file state), then `N:<verbatim file content>` per line. Everything after `N:` is exact file text including indentation (tabs vs spaces) — to edit, strip only the `N:` prefix and reproduce the rest byte-for-byte in oldText; never line numbers or :N selectors. Continuation works via offset=N exactly as the notices say.";

export default function (pi: ExtensionAPI) {
	// The built-in definition supplies the schema, output schema, prompt
	// contribution, renderers, and the image-reading execute we delegate to.
	const builtin = createReadToolDefinition(process.cwd());

	pi.registerTool({
		...builtin,
		name: "read",
		label: "read",
		description: `${builtin.description}${DESCRIPTION_EXTENSION}`,
		async execute(toolCallId, params, signal, onUpdate, ctx) {
			signal?.throwIfAborted(); // parity with the built-in read
			const cwd = ctx?.cwd ?? process.cwd();
			const abs = resolveReadPath(params.path, cwd);

			// Image branch: let the built-in read handle detection, (optional)
			// resizing, and the image content block.
			const mime = await detectSupportedImageMimeTypeFromFile(abs).catch(() => null);
			if (mime) return builtin.execute(toolCallId, params, signal, onUpdate, ctx);

			// Text branch: slice by offset/limit FIRST, then number the lines, then
			// truncate the numbered text, then prepend the fingerprint header.
			// Forked from the built-in read.js pipeline (slice -> prefix -> truncate
			// ordering is the one hard constraint) — re-sync if pi changes it.
			const text = BOM_STRIP((await fs.readFile(abs, "utf8")) as string);
			const tag = fileHash(text);
			const allLines = text.split("\n");
			const totalFileLines = allLines.length;
			const startLine = params.offset ? Math.max(0, params.offset - 1) : 0;
			const startLineDisplay = startLine + 1;
			if (startLine >= allLines.length) {
				throw new Error(`Offset ${params.offset} is beyond end of file (${allLines.length} lines total)`);
			}

			let selected: string[];
			let userLimitedLines: number | undefined;
			if (params.limit !== undefined) {
				const endLine = Math.min(startLine + params.limit, allLines.length);
				selected = allLines.slice(startLine, endLine);
				userLimitedLines = endLine - startLine;
			} else {
				selected = allLines.slice(startLine);
			}

			const numbered = formatNumberedLines(selected.join("\n"), startLineDisplay);
			const truncation = truncateHead(numbered);
			const header = formatHeader(displayPath(abs, cwd), tag);
			let outputText: string;
			let details: { truncation?: typeof truncation } = {};

			if (truncation.firstLineExceedsLimit) {
				// First line alone exceeds the byte limit. Point the model at a bash fallback.
				// Size reported for the N:-prefixed line (the flag is raised on it).
				const firstLineSize = formatSize(Buffer.byteLength(`${startLineDisplay}:${allLines[startLine]}`, "utf-8"));
				outputText = `[Line ${startLineDisplay} is ${firstLineSize}, exceeds ${formatSize(DEFAULT_MAX_BYTES)} limit. Use bash: sed -n '${startLineDisplay}p' ${params.path} | head -c ${DEFAULT_MAX_BYTES}]`;
				details = { truncation };
			} else if (truncation.truncated) {
				// Truncation occurred. Build an actionable continuation notice.
				const endLineDisplay = startLineDisplay + truncation.outputLines - 1;
				const nextOffset = endLineDisplay + 1;
				outputText = `${header}\n${truncation.content}`;
				if (truncation.truncatedBy === "lines") {
					outputText += `\n\n[Showing lines ${startLineDisplay}-${endLineDisplay} of ${totalFileLines}. Use offset=${nextOffset} to continue.]`;
				} else {
					outputText += `\n\n[Showing lines ${startLineDisplay}-${endLineDisplay} of ${totalFileLines} (${formatSize(DEFAULT_MAX_BYTES)} limit). Use offset=${nextOffset} to continue.]`;
				}
				details = { truncation };
			} else if (userLimitedLines !== undefined && startLine + userLimitedLines < allLines.length) {
				// User-specified limit stopped early, but the file still has more content.
				const remaining = allLines.length - (startLine + userLimitedLines);
				const nextOffset = startLine + userLimitedLines + 1;
				outputText = `${header}\n${truncation.content}\n\n[${remaining} more lines in file. Use offset=${nextOffset} to continue.]`;
			} else {
				// No truncation and no remaining user-limited content.
				outputText = `${header}\n${truncation.content}`;
			}

			return {
				content: [{ type: "text", text: outputText }],
				details,
				structuredContent: outputText,
			};
		},
	});
}
