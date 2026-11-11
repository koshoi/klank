/**
 * hashline.ts - omp-style hashline formatting primitives (pure, no pi imports).
 *
 * Inspired by the omp (Oh My Pi) coding-agent harness's read format
 * (https://github.com/can1357/oh-my-pi — packages/tui/src/tools/hashline-format.ts):
 * a `[path#TAG]` content-hash header plus 1-based numbered lines. Simplified
 * for pi: the tag is display-only (pi's edit tool matches exact oldText and
 * does not consume tags), and no native hashing is involved.
 *
 * The 4-hex `TAG` in a read header (`[path#TAG]`) is a content fingerprint of
 * the file as it was read: the first 4 uppercase hex digits of sha256 over the
 * BOM-stripped, CRLF→LF-normalized text. edit-guard's 8-hex snapshot hash is
 * computed over the same normalization, so the tag is always a prefix of it —
 * but the tag itself is DISPLAY-ONLY; freshness gating stays on the full hash.
 */

import * as crypto from "node:crypto";
import * as os from "node:os";
import * as path from "node:path";

/**
 * Loader entrypoint: this is a pure helper module, nothing to register.
 * (The pi extension loader requires every .ts in the extensions dir to export
 * a factory function; consumers import the named helpers above directly.)
 */
export default function () {}

/** 4 uppercase hex digits of sha256 over BOM-stripped, CRLF-normalized text. */
export const fileHash = (text: string): string => {
	const normalized = text.replace(/^\uFEFF/, "").replace(/\r\n/g, "\n");
	return crypto.createHash("sha256").update(normalized).digest("hex").slice(0, 4).toUpperCase();
};

/**
 * Prefix every line with its 1-based source line number: "3:x\n4:y".
 * `startLine` is the 1-based line number of the first line of `text`, so
 * numbering stays stable across continuation reads (offset/limit).
 */
export const formatNumberedLines = (text: string, startLine = 1): string =>
	text
		.split("\n")
		.map((line, i) => `${startLine + i}:${line}`)
		.join("\n");

/** `[path#TAG]` — the header prepended to read output. */
export const formatHeader = (displayPath: string, tag: string): string =>
	`[${displayPath}#${tag}]`;

/**
 * Path as shown to the model: relative to `cwd` when the file lives under it
 * (with `..` segments), otherwise a ~-shortened absolute path.
 */
export const displayPath = (abs: string, cwd: string): string => {
	const rel = path.relative(cwd, abs);
	if (rel !== "" && !rel.startsWith("..") && !path.isAbsolute(rel)) return rel;
	const home = os.homedir();
	if (abs === home) return "~";
	if (abs.startsWith(home + path.sep)) return "~" + abs.slice(home.length);
	return abs;
};
