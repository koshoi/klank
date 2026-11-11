/**
 * Tests for fuzzy-edit.ts pure helpers (whitespace-insensitive matching,
 * re-indentation) and edit-guard.ts seen-lines bookkeeping.
 *
 * Run: node --test --experimental-strip-types extensions/fuzzy-edit.test.mjs
 */

import assert from "node:assert/strict";
import { before, describe, it } from "node:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import {
	buildIndentProfile,
	describeIndent,
	trimMatchPositions,
	adjustNewText,
	visibleWhitespace,
	resolveEditSpans,
	whitespaceFallback,
} from "./fuzzy-edit.ts";
import { parseSeenLines } from "./edit-guard.ts";

describe("trimMatchPositions", () => {
	const file = ["function a() {", "\tfoo();", "\tbar();", "}"].join("\n").split("\n");

	it("finds a space-indented pattern in a tab-indented file", () => {
		const pattern = "function a() {\n    foo();\n    bar();\n}".split("\n");
		assert.deepEqual(trimMatchPositions(file, pattern), [0]);
	});

	it("finds nothing when content differs", () => {
		const pattern = "function b() {\n    foo();\n}".split("\n");
		assert.deepEqual(trimMatchPositions(file, pattern), []);
	});

	it("reports multiple matches so the caller can reject ambiguity", () => {
		const lines = ["foo", "  bar", "baz", "bar"];
		assert.deepEqual(trimMatchPositions(lines, ["bar"]), [1, 3]);
	});

	it("ignores trailing whitespace", () => {
		assert.deepEqual(trimMatchPositions(["foo   ", "bar\t\t"], ["foo", "bar"]), [0]);
	});
});

describe("buildIndentProfile", () => {
	it("detects tab-only files", () => {
		const p = buildIndentProfile("if (x) {\n\tfoo();\n}");
		assert.equal(p.tabOnly, true);
		assert.equal(p.unit, 1);
		assert.equal(describeIndent(p), "tabs");
	});

	it("detects space-only files with GCD unit", () => {
		const p = buildIndentProfile("if (x) {\n    foo();\n        bar();\n  }");
		assert.equal(p.spaceOnly, true);
		assert.equal(p.unit, 2);
		assert.equal(describeIndent(p), "spaces (2 per level)");
	});

	it("flags mixed indentation", () => {
		const p = buildIndentProfile("if (x) {\n\t  foo();\n}");
		assert.equal(p.mixed, true);
		assert.equal(describeIndent(p), "mixed tabs+spaces");
	});
});

describe("adjustNewText", () => {
	it("converts model spaces to file tabs", () => {
		const r = adjustNewText(
			"if (x) {\n    foo();\n}",
			"if (x) {\n\tfoo();\n}",
			"if (x) {\n    baz();\n}",
		);
		assert.equal(r.text, "if (x) {\n\tbaz();\n}");
		assert.match(r.note ?? "", /tabs/);
	});

	it("converts model tabs to file spaces (unit from file)", () => {
		const r = adjustNewText(
			"if (x) {\n\tfoo();\n}",
			"if (x) {\n    foo();\n}",
			"if (x) {\n\tbaz();\n\tqux();\n}",
		);
		assert.equal(r.text, "if (x) {\n    baz();\n    qux();\n}");
	});

	it("applies a uniform nesting delta", () => {
		const r = adjustNewText(
			"foo();\nbar();",
			"    foo();\n    bar();",
			"baz();",
		);
		assert.equal(r.text, "    baz();");
	});

	it("leaves text alone when indentation is inconsistent", () => {
		const r = adjustNewText("foo();", "    foo();", "\tbaz();");
		assert.equal(r.text, "\tbaz();"); // tab-indented newText vs space-indented file: untouched
	});

	it("re-indents by the uniform delta even when oldText is unindented", () => {
		const r = adjustNewText("foo();", "\tfoo();", "baz();");
		assert.equal(r.text, "\tbaz();"); // matched one nesting level deep -> shifted to match
		assert.ok(r.note);
	});
});

describe("visibleWhitespace", () => {
	it("shows tabs and trailing spaces", () => {
		assert.equal(visibleWhitespace("\tfoo()  "), "->foo()··");
		assert.equal(visibleWhitespace("foo()"), "foo()");
	});
});

describe("resolveEditSpans", () => {
	const content = ["one", "two", "three", "four"].join("\n");

	it("resolves exact matches with line spans", () => {
		const spans = resolveEditSpans(content, [{ oldText: "two\nthree", newText: "x" }]);
		assert.ok(spans);
		assert.equal(spans.length, 1);
		assert.equal(spans[0].startLine, 1);
		assert.equal(spans[0].endLine, 3);
	});

	it("resolves whitespace-insensitive matches", () => {
		const spans = resolveEditSpans(content, [{ oldText: "  two\n  three", newText: "x" }]);
		assert.ok(spans);
		assert.equal(spans[0].startLine, 1);
	});

	it("returns null for ambiguous or missing blocks", () => {
		assert.equal(resolveEditSpans("a\nb\na", [{ oldText: "a", newText: "c" }]), null);
		assert.equal(resolveEditSpans("a\nb", [{ oldText: "zzz", newText: "c" }]), null);
	});
});

describe("whitespaceFallback (live file round-trip)", () => {
	let dir;
	before(() => {
		dir = fs.mkdtempSync(path.join(os.tmpdir(), "fuzzy-edit-"));
	});

	it("matches a space-indented oldText against a tab-indented file and re-indents newText", () => {
		const file = path.join(dir, "tab.ts");
		fs.writeFileSync(file, "function a() {\n\tfoo();\n\tbar();\n}\n");
		const result = whitespaceFallback(
			{ path: file, edits: [{ oldText: "function a() {\n    foo();\n    bar();\n}", newText: "function a() {\n    baz();\n}" }] },
			dir,
			"Could not find the exact text in x.",
		);
		assert.match(result.content[0].text, /whitespace-insensitive/);
		assert.match(result.content[0].text, /tabs/);
		assert.equal(fs.readFileSync(file, "utf8"), "function a() {\n\tbaz();\n}\n");
	});

	it("preserves CRLF and BOM on write", () => {
		const file = path.join(dir, "crlf.ts");
		fs.writeFileSync(file, "\uFEFFa\r\n\tb\r\n");
		whitespaceFallback(
			{ path: file, edits: [{ oldText: "a\n    b", newText: "a\n    c" }] },
			dir,
			"Could not find the exact text in x.",
		);
		const out = fs.readFileSync(file, "utf8");
		assert.ok(out.startsWith("\uFEFF"));
		assert.ok(out.includes("\r\n"));
		assert.ok(out.includes("\tc\r\n"));
	});

	it("throws the whitespace-specific error with visible whitespace when nothing matches", () => {
		const file = path.join(dir, "miss.ts");
		fs.writeFileSync(file, "function a() {\n\tfoo();\n}\n");
		assert.throws(
			() => whitespaceFallback(
				{ path: file, edits: [{ oldText: "function z() {\n    qux();\n}", newText: "x" }] },
				dir,
				"Could not find the exact text in x.",
			),
			(err) => {
				assert.match(err.message, /content differs too/); // 33% similar: not a whitespace-only claim
				assert.match(err.message, /file uses tabs/);
				assert.match(err.message, /->foo/); // visible tab
				return true;
			},
		);
	});

	it("tells the model when no similar region exists at all", () => {
		const file = path.join(dir, "nomatch.ts");
		fs.writeFileSync(file, "aaa\nbbb\n");
		assert.throws(
			() => whitespaceFallback(
				{ path: file, edits: [{ oldText: "xxx\nyyy", newText: "z" }] },
				dir,
				"Could not find the exact text in x.",
			),
			(err) => {
				assert.match(err.message, /No similar region exists/);
				return true;
			},
		);
	});

	it("refuses ambiguous whitespace-insensitive matches", () => {
		const file = path.join(dir, "ambig.ts");
		fs.writeFileSync(file, "foo\n\tbar\nbaz\nbar\n");
		assert.throws(
			() => whitespaceFallback(
				{ path: file, edits: [{ oldText: "bar", newText: "x" }] },
				dir,
				"Could not find the exact text in x.",
			),
			/unique/,
		);
	});
});

describe("parseSeenLines (edit-guard)", () => {
	it("parses N:-prefixed lines from read output", () => {
		const seen = parseSeenLines([
			{ type: "text", text: "[src/foo.ts#A1B2]\n1:import x\n2:y\n3:z" },
		]);
		assert.ok(seen);
		assert.equal(seen.all, true);
		assert.deepEqual([...seen.lines].sort((a, b) => a - b), [1, 2, 3]);
	});

	it("marks partial reads as not-all, full footer reads as all", () => {
		const partial = parseSeenLines([
			{ type: "text", text: "[f#A1B2]\n5:a\n6:b\n[Showing lines 5-6 of 340. Use offset=7 to continue.]" },
		]);
		assert.ok(partial);
		assert.equal(partial.all, false);

		const throughEof = parseSeenLines([
			{ type: "text", text: "[f#A1B2]\n339:a\n340:b\n[Showing lines 339-340 of 340. Use offset=341 to continue.]" },
		]);
		assert.ok(throughEof);
		assert.equal(throughEof.all, true);
	});

	it("returns null for empty/image-only results", () => {
		assert.equal(parseSeenLines(undefined), null);
		assert.equal(parseSeenLines([{ type: "image", text: undefined }]), null);
	});
});
