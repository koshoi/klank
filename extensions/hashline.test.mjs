/**
 * Tests for hashline.ts (pure module) and the edit-guard extension's live
 * snapshot/reveal behavior (live guard harness with a stubbed pi API).
 *
 * Run: node --test --experimental-strip-types extensions/hashline.test.mjs
 * (node 24 strips types, so the .ts modules are imported directly)
 */

import assert from "node:assert/strict";
import * as crypto from "node:crypto";
import { after, before, describe, it } from "node:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import { displayPath, fileHash, formatHeader, formatNumberedLines } from "./hashline.ts";
import { resolveReadPath } from "./hashline-read.ts";

const HEX4 = /^[0-9A-F]{4}$/;

describe("fileHash", () => {
	it("returns 4 uppercase hex digits", () => {
		for (const text of ["", "hello", "line1\nline2\n"]) {
			assert.match(fileHash(text), HEX4);
		}
	});

	it("is deterministic and input-sensitive", () => {
		assert.equal(fileHash("abc"), fileHash("abc"));
		assert.notEqual(fileHash("abc"), fileHash("abd"));
	});

	it("is CRLF-insensitive (CRLF normalizes to LF)", () => {
		assert.equal(fileHash("a\r\nb\r\n"), fileHash("a\nb\n"));
	});

	it("is BOM-insensitive", () => {
		assert.equal(fileHash("\uFEFFa\nb\n"), fileHash("a\nb\n"));
	});

	it("matches the first 4 hex of sha256 over normalized text", () => {
		const expected = crypto
			.createHash("sha256")
			.update("a\nb\n")
			.digest("hex")
			.slice(0, 4)
			.toUpperCase();
		assert.equal(fileHash("a\r\nb\r\n"), expected);
	});
});

describe("formatNumberedLines", () => {
	it("prefixes 1-based line numbers by default", () => {
		assert.equal(formatNumberedLines("x\ny"), "1:x\n2:y");
	});

	it("honors the start line (continuation reads)", () => {
		assert.equal(formatNumberedLines("x\ny", 3), "3:x\n4:y");
	});

	it("keeps empty lines and trailing newlines stable", () => {
		assert.equal(formatNumberedLines("a\n\nb", 5), "5:a\n6:\n7:b");
	});
});

describe("formatHeader / displayPath", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "hashline-test-"));
	const cwd = path.join(tmp, "proj");
	const sub = path.join(cwd, "src");

	before(() => {
		fs.mkdirSync(sub, { recursive: true });
		fs.writeFileSync(path.join(sub, "a.ts"), "export {};\n");
	});

	after(() => fs.rmSync(tmp, { recursive: true, force: true }));

	it("formatHeader renders [path#TAG]", () => {
		assert.equal(formatHeader("src/a.ts", "A1B2"), "[src/a.ts#A1B2]");
	});

	it("displayPath is cwd-relative for files under cwd", () => {
		assert.equal(displayPath(path.join(sub, "a.ts"), cwd), "src/a.ts");
	});

	it("displayPath falls back to ~-shortened absolute for siblings", () => {
		const sibling = path.join(path.dirname(cwd), "other.ts");
		assert.equal(displayPath(sibling, cwd), sibling.replace(os.homedir(), "~"));
		assert.ok(displayPath(sibling, cwd).startsWith("~/") || path.isAbsolute(sibling));
	});

	it("displayPath returns absolute paths outside home as-is", () => {
		assert.equal(displayPath("/etc/hosts", cwd), "/etc/hosts");
	});
});


describe("resolveReadPath (built-in parity)", () => {
	const cwd = "/tmp/resolve-cwd";
	const home = process.env.HOME;

	it("strips the @ mention prefix (built-in: stripAtPrefix)", () => {
		assert.equal(resolveReadPath("@src/foo.ts", cwd), path.resolve(cwd, "src/foo.ts"));
	});

	it("collapses unicode spaces to plain spaces", () => {
		assert.equal(resolveReadPath("my\u00A0file.ts", cwd), path.resolve(cwd, "my file.ts"));
	});

	it("expands ~ and ~/sub against HOME", () => {
		assert.equal(resolveReadPath("~", cwd), home);
		assert.equal(resolveReadPath("~/x.ts", cwd), path.join(home, "x.ts"));
	});

	it("resolves relative paths against cwd and keeps absolutes", () => {
		assert.equal(resolveReadPath("a/b.ts", cwd), path.resolve(cwd, "a/b.ts"));
		assert.equal(resolveReadPath("/etc/hosts", cwd), "/etc/hosts");
	});

	it("converts file:// URLs", () => {
		assert.equal(resolveReadPath("file:///etc/hosts", cwd), "/etc/hosts");
	});
});

// --- live guard harness: edit-guard with a stubbed pi API --------------------

describe("edit-guard live harness", async () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "edit-guard-test-"));
	const file = path.join(dir, "sample.ts");
	const initial = "alpha\nbeta\ngamma\n";

	// Stubbed ExtensionAPI: collect handlers (multiple per event), expose the
	// guard's tool_call results.
	const handlers = new Map();
	const pi = {
		on: (event, handler) => {
			const list = handlers.get(event) ?? [];
			list.push(handler);
			handlers.set(event, list);
		},
		registerCommand: () => {},
		registerTool: () => {},
		getAllTools: () => [{ name: "read" }, { name: "edit" }, { name: "write" }, { name: "bash" }],
	};
	const callTool = (toolName, input) =>
		handlers.get("tool_call")[0]({
			toolName,
			input,
			isSynthetic: false,
		});

	const { default: editGuard } = await import("./edit-guard.ts");
	editGuard(pi);
	assert.ok(handlers.has("tool_call"), "guard registered a tool_call handler");
	assert.ok(handlers.has("tool_result"), "guard registered a tool_result handler");

	it("(setup) file exists; a read tool_result records a snapshot with a tag", async () => {
		fs.writeFileSync(file, initial);
		await handlers.get("tool_result")[0]({
			toolName: "read",
			input: { path: file },
			isError: false,
		});
		// The snapshot itself is private; verify indirectly: an edit against the
		// unchanged file is NOT blocked (snapshot exists and matches).
		const result = await callTool("edit", { path: file, oldText: "alpha", newText: "ALPHA" });
		assert.equal(result, undefined, "matching snapshot must not block");
	});

	it("(a) blocks an out-of-band edit with both 4-hex tags and the LCS reveal", async () => {
		fs.writeFileSync(file, "alpha\nbeta\nGAMMA\n"); // out-of-band mutation
		const result = await callTool("edit", { path: file, oldText: "beta", newText: "BETA" });
		assert.ok(result?.block, "stale edit must be blocked");
		const match = result.reason.match(/snapshot ([0-9A-F]{4}), on disk now ([0-9A-F]{4})/);
		assert.ok(match, `block reason quotes two 4-hex tags: ${result.reason}`);
		assert.notEqual(match[1], match[2], "the two tags differ");
		assert.ok(result.reason.includes("@@"), "block reason includes the LCS reveal");
		assert.ok(result.reason.includes("- gamma"), "reveal shows the old line");
		assert.ok(result.reason.includes("+ GAMMA"), "reveal shows the new line");
	});

	it("(b) reveal refresh prevents a second block", async () => {
		const result = await callTool("edit", { path: file, oldText: "beta", newText: "BETA" });
		assert.equal(result, undefined, "second attempt must not be blocked again");
	});

	it("(c) guard respects bash interceptor and edit-guard prompt rule is extended", async () => {
		const bash = await callTool("bash", { command: "sed -i 's/a/b/' x.ts" });
		assert.ok(bash?.block, "sed -i is intercepted");
		// prompt rule (registered by the guard during editGuard(pi))
		const event = { systemPromptOptions: { promptGuidelines: [] } };
		await handlers.get("before_agent_start")[0](event);
		const rule = event.systemPromptOptions.promptGuidelines.at(-1);
		assert.ok(rule.includes("[path#TAG]"), "prompt rule mentions the tag header");
		assert.ok(rule.includes("exact oldText"), "prompt rule mentions exact oldText");
	});
});
