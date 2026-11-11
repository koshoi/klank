/**
 * Vim Editor — real modal editing for pi's input editor.
 *
 * Replaces the toy modal-editor example. Motion/verb coverage:
 *
 *   motions   h l j k w W b B e E 0 ^ $ gg G { } f t F T % ; ,
 *   counts    all motions/operators take counts (3j, d2w, 2dd, y5l, ...)
 *   operators d c y (+ doubled linewise dd cc yy, and d/c/y + any motion)
 *   edits     x X r ~ s S D C J o O p P
 *   undo      u / ctrl+r (redo; cooperates with the editor's ctrl+- undo)
 *   insert    i a I A (and everything above that ends in insert mode)
 *
 * Mode indicator: label at the start of the prompt line
 * (NORMAL / INSERT / pending operator, bold black on colored bg)
 * plus a cursor shape change (block = normal, beam = insert).
 *
 * Everything not handled here (Enter, ctrl+c, tab/autocomplete, paste,
 * history browsing at editor edges) is passed through to the base editor.
 */

import { CustomEditor, type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { matchesKey, truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";

type Pos = { line: number; col: number };
type MotionResult = { pos: Pos; linewise?: boolean; inclusive?: boolean };

const WORD = /[A-Za-z0-9_]/;

function isSpace(ch: string | undefined): boolean {
	return ch !== undefined && /\s/.test(ch);
}

function charClass(ch: string | undefined): "word" | "space" | "punct" {
	if (ch === undefined || /\s/.test(ch)) return "space";
	return WORD.test(ch) ? "word" : "punct";
}

export class VimEditor extends CustomEditor {
	private mode: "normal" | "insert" = "insert";
	private count1: number | null = null; // count typed before operator
	private count2: number | null = null; // count typed after operator
	private op: "d" | "c" | "y" | null = null;
	private pendingFind: { type: "f" | "t" | "F" | "T"; count: number } | null = null;
	private pendingReplace = false;
	private pendingG = false;
	private lastFind: { type: "f" | "t" | "F" | "T"; char: string; count: number } | null = null;
	private register: { text: string; linewise: boolean } | null = null;
	private redoStack: { lines: string[]; line: number; col: number }[] = [];

	constructor(...args: ConstructorParameters<typeof CustomEditor>) {
		super(...args);
	}

	// --- access to Editor internals that are TS-private but runtime-stable ---

	private get int(): any {
		return this;
	}

	// --- state helpers ---

	private lines(): string[] {
		return this.getLines();
	}

	private cur(): Pos {
		return this.getCursor();
	}

	private snap(): void {
		this.int.pushUndoSnapshot();
	}

	private apply(lines: string[], cursor: Pos, keepRedo = false): void {
		this.int.state.lines = lines;
		this.int.state.cursorLine = cursor.line;
		this.int.setCursorCol(cursor.col);
		this.int.cancelAutocomplete();
		if (!keepRedo) this.redoStack.length = 0;
	}

	private snapNow(): { lines: string[]; line: number; col: number } {
		const p = this.cur();
		return { lines: this.lines(), line: p.line, col: p.col };
	}

	private restore(s: { lines: string[]; line: number; col: number }): void {
		this.apply(s.lines, { line: s.line, col: s.col }, true);
	}

	private enterInsert(): void {
		this.setMode("insert");
		this.redoStack.length = 0; // new edits invalidate redo history
	}

	private setMode(mode: "normal" | "insert"): void {
		this.mode = mode;
		this.count1 = null;
		this.count2 = null;
		this.op = null;
		this.pendingFind = null;
		this.pendingReplace = false;
		this.pendingG = false;
		// Block cursor in normal mode, beam in insert mode (DECSCUSR).
		process.stdout.write(mode === "normal" ? "\x1b[2 q" : "\x1b[6 q");
		this.int.tui.requestRender?.();
	}

	// ------------------------------------------------------------------
	// Motion engine (pure functions over the lines array)
	// ------------------------------------------------------------------

	private clampCol(line: number, col: number): number {
		return Math.max(0, Math.min(col, (this.lines()[line] ?? "").length));
	}

	/** Clamp a normal-mode cursor onto the last character of the line. */
	private normalClamp(p: Pos): Pos {
		const len = (this.lines()[p.line] ?? "").length;
		return { line: p.line, col: len === 0 ? 0 : Math.min(p.col, len - 1) };
	}

	private wordForward(p: Pos, big: boolean): Pos {
		const lines = this.lines();
		let { line, col } = p;
		for (let guard = 0; guard < 100000; guard++) {
			const ch = lines[line]?.[col];
			if (ch === undefined) {
				// past end of line -> next line start
				if (line + 1 < lines.length) {
					line++;
					col = 0;
					if (!isSpace(lines[line][0])) return { line, col };
					continue;
				}
				return { line, col: Math.max(0, (lines[line]?.length ?? 1) - 1) };
			}
			if (isSpace(ch)) {
				col++;
				continue;
			}
			// skip the current class run
			const cls = big ? "big" : charClass(ch);
			while (true) {
				const c = lines[line]?.[col];
				if (c === undefined || (big ? isSpace(c) : charClass(c) !== cls)) break;
				col++;
			}
			// skip following whitespace (possibly across lines)
			while (true) {
				const c = lines[line]?.[col];
				if (c === undefined) {
					if (line + 1 < lines.length) {
						line++;
						col = 0;
						if (!isSpace(lines[line][0])) return { line, col };
						continue;
					}
					return { line, col: Math.max(0, (lines[line]?.length ?? 1) - 1) };
				}
				if (isSpace(c)) {
					col++;
					continue;
				}
				return { line, col };
			}
		}
		return p;
	}

	private wordBackward(p: Pos, big: boolean): Pos {
		const lines = this.lines();
		let { line, col } = p;
		for (let guard = 0; guard < 100000; guard++) {
			if (col === 0) {
				if (line === 0) return p;
				line--;
				if ((lines[line] ?? "").length === 0) return { line, col: 0 };
				col = lines[line].length;
				continue;
			}
			col--;
			const ch = lines[line]?.[col];
			if (ch === undefined) return { line, col: 0 };
			if (isSpace(ch)) continue;
			const cls = big ? "big" : charClass(ch);
			while (col > 0) {
				const prev = lines[line][col - 1];
				if (big ? isSpace(prev) : charClass(prev) !== cls) break;
				col--;
			}
			return { line, col };
		}
		return p;
	}

	private wordEnd(p: Pos, big: boolean): Pos {
		const lines = this.lines();
		let { line, col } = p;
		for (let guard = 0; guard < 100000; guard++) {
			col++;
			let ch = lines[line]?.[col];
			if (ch === undefined) {
				if (line + 1 >= lines.length) return this.normalClamp({ line, col: Math.max(0, col - 1) });
				line++;
				col = 0;
				ch = lines[line][0];
				if (ch === undefined) return { line, col: 0 };
			}
			if (isSpace(ch)) continue;
			const cls = big ? "big" : charClass(ch);
			while (true) {
				const next = lines[line]?.[col + 1];
				if (next === undefined || (big ? isSpace(next) : charClass(next) !== cls)) break;
				col++;
			}
			return { line, col };
		}
		return p;
	}

	private firstNonBlank(line: number): number {
		const text = this.lines()[line] ?? "";
		const m = text.match(/\S/);
		return m ? m.index! : 0;
	}

	private findCharInLine(pos: Pos, type: "f" | "t" | "F" | "T", ch: string, count: number): Pos | null {
		const text = this.lines()[pos.line] ?? "";
		if (type === "f" || type === "t") {
			let idx = pos.col;
			for (let n = 0; n < count; n++) {
				idx = text.indexOf(ch, idx + 1);
				if (idx === -1) return null;
			}
			return type === "t" ? (idx > pos.col + 1 ? { line: pos.line, col: idx - 1 } : null) : { line: pos.line, col: idx };
		}
		let idx = pos.col;
		for (let n = 0; n < count; n++) {
			idx = text.lastIndexOf(ch, idx - 1);
			if (idx === -1) return null;
		}
		return type === "T" ? (idx < pos.col - 1 ? { line: pos.line, col: idx + 1 } : null) : { line: pos.line, col: idx };
	}

	private matchBracket(p: Pos): Pos | null {
		const lines = this.lines();
		const pairs: Record<string, string> = { "(": ")", "[": "]", "{": "}" };
		const reverse: Record<string, string> = { ")": "(", "]": "[", "}": "{" };
		// find the first bracket at/after the cursor (scans forward across lines)
		let { line, col } = p;
		let open = "";
		for (let guard = 0; guard < 100000; guard++) {
			const ch = lines[line]?.[col];
			if (ch === undefined) {
				if (line + 1 >= lines.length) return null;
				line++;
				col = 0;
				continue;
			}
			if (pairs[ch] || reverse[ch]) {
				open = ch;
				break;
			}
			col++;
		}
		if (!open) return null;
		const forward = open in pairs;
		const target = forward ? pairs[open] : reverse[open];
		let depth = 1; // standing on the starting bracket
		for (let guard = 0; guard < 1000000; guard++) {
			// advance one char in the search direction, then depth-count
			// (stepping off the starting bracket first, so it isn't counted)
			if (forward) {
				col++;
				if (col > (lines[line]?.length ?? 0)) {
					line++;
					col = 0;
					if (line >= lines.length) return null;
				}
			} else {
				col--;
				if (col < 0) {
					line--;
					if (line < 0) return null;
					col = Math.max(0, (lines[line]?.length ?? 1) - 1);
				}
			}
			const ch = lines[line]?.[col];
			if (ch === open) depth++;
			else if (ch === target) {
				depth--;
				if (depth === 0) return { line, col };
			}
		}
		return null;
	}

	/** Resolve a motion key in normal/operator-pending mode. */
	private motion(data: string, count: number, hasCount = false): MotionResult | null {
		const p = this.cur();
		const lines = this.lines();
		switch (data) {
			case "h":
				return { pos: { line: p.line, col: Math.max(0, p.col - count) } };
			case "l":
				return { pos: { line: p.line, col: Math.min((lines[p.line] ?? "").length, p.col + count) } };
			case "j": {
				const line = Math.min(lines.length - 1, p.line + count);
				return { pos: this.normalClamp({ line, col: p.col }), linewise: p.line !== line };
			}
			case "k": {
				const line = Math.max(0, p.line - count);
				return { pos: this.normalClamp({ line, col: p.col }), linewise: p.line !== line };
			}
			case "w": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordForward(pos, false);
				return { pos };
			}
			case "W": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordForward(pos, true);
				return { pos };
			}
			case "b": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordBackward(pos, false);
				return { pos };
			}
			case "B": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordBackward(pos, true);
				return { pos };
			}
			case "e": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordEnd(pos, false);
				return { pos, inclusive: true };
			}
			case "E": {
				let pos = p;
				for (let i = 0; i < count; i++) pos = this.wordEnd(pos, true);
				return { pos, inclusive: true };
			}
			case "0":
				return { pos: { line: p.line, col: 0 } };
			case "^":
				return { pos: { line: p.line, col: this.firstNonBlank(p.line) } };
			case "$": {
				const line = Math.min(lines.length - 1, p.line + count - 1);
				return { pos: this.normalClamp({ line, col: (lines[line] ?? "").length }) };
			}
			case "{": {
				const line = Math.max(0, p.line - count);
				let target = p.line;
				for (let i = p.line - 1; i >= line; i--) {
					if (!lines[i]?.trim()) {
						target = i;
						break;
					}
					target = i;
				}
				return { pos: { line: target, col: 0 }, linewise: target !== p.line };
			}
			case "}": {
				let target = p.line;
				for (let i = p.line + 1; i <= Math.min(lines.length - 1, p.line + count); i++) {
					target = i;
					if (!lines[i]?.trim()) break;
				}
				return { pos: { line: target, col: 0 }, linewise: target !== p.line };
			}
			case "G": {
				const line = hasCount ? Math.min(lines.length - 1, count - 1) : lines.length - 1;
				return { pos: this.normalClamp({ line, col: p.col }), linewise: line !== p.line };
			}
			default:
				if (data === "f" || data === "t" || data === "F" || data === "T") return null; // handled as pending-char
				return null;
		}
	}

	// ------------------------------------------------------------------
	// Operators
	// ------------------------------------------------------------------

	private rangeText(lines: string[], start: Pos, end: Pos, linewise: boolean): { text: string; linewise: boolean } {
		if (linewise) {
			return { text: lines.slice(start.line, end.line + 1).join("\n"), linewise: true };
		}
		if (start.line === end.line) return { text: lines[start.line].slice(start.col, end.col), linewise: false };
		const parts = [lines[start.line].slice(start.col), ...lines.slice(start.line + 1, end.line), lines[end.line].slice(0, end.col)];
		return { text: parts.join("\n"), linewise: false };
	}

	private deleteCharwise(start: Pos, end: Pos): void {
		const lines = this.lines();
		this.snap();
		let newLines: string[];
		if (start.line === end.line) {
			const text = lines[start.line];
			newLines = [...lines];
			newLines[start.line] = text.slice(0, start.col) + text.slice(end.col);
		} else {
			const merged = (lines[start.line] ?? "").slice(0, start.col) + (lines[end.line] ?? "").slice(end.col);
			newLines = [...lines.slice(0, start.line), merged, ...lines.slice(end.line + 1)];
		}
		this.apply(newLines, { line: start.line, col: Math.min(start.col, (newLines[start.line] ?? "").length) });
	}

	private deleteLinewise(fromLine: number, toLine: number): void {
		const lines = this.lines();
		this.snap();
		const newLines = lines.slice(0, fromLine).concat(lines.slice(toLine + 1));
		if (newLines.length === 0) newLines.push("");
		const line = Math.min(fromLine, newLines.length - 1);
		this.apply(newLines, { line, col: this.firstNonBlank(line) });
	}

	private yank(start: Pos, end: Pos, linewise: boolean): void {
		this.register = this.rangeText(this.lines(), start, end, linewise);
	}

	private applyOperator(op: "d" | "c" | "y", start: Pos, end: Pos, linewise: boolean): void {
		if (op === "y") {
			this.yank(start, end, linewise);
			if (!linewise) this.apply(this.lines(), start);
			return;
		}
		if (linewise) {
			const text = this.rangeText(this.lines(), start, end, true);
			this.register = { text, linewise: true };
			this.deleteLinewise(start.line, end.line);
		} else {
			const text = this.rangeText(this.lines(), start, end, false);
			this.register = { text, linewise: false };
			this.deleteCharwise(start, end);
		}
		if (op === "c") this.enterInsert();
	}

	// ------------------------------------------------------------------
	// Paste / join / replace helpers
	// ------------------------------------------------------------------

	private paste(after: boolean, count: number): void {
		const reg = this.register;
		if (!reg) return;
		const lines = this.lines();
		const p = this.cur();
		this.snap();
		if (reg.linewise) {
			const at = after ? p.line + 1 : p.line;
			const block: string[] = [];
			for (let i = 0; i < count; i++) block.push(...reg.text.split("\n"));
			const newLines = [...lines.slice(0, at), ...block, ...lines.slice(at)];
			this.apply(newLines, { line: at, col: this.firstNonBlank(at) });
		} else {
			const insertCol = Math.min((lines[p.line] ?? "").length, p.col + (after ? 1 : 0));
			const before = (lines[p.line] ?? "").slice(0, insertCol);
			const afterText = (lines[p.line] ?? "").slice(insertCol);
			const parts = reg.text.split("\n");
			const newLines = [...lines];
			if (parts.length === 1) {
				newLines[p.line] = before + parts[0].repeat(count) + afterText;
				this.apply(newLines, { line: p.line, col: insertCol + parts[0].length * count - 1 });
			} else {
				const mid = [...parts];
				mid[0] = before + mid[0];
				mid[mid.length - 1] = mid[mid.length - 1] + afterText;
				newLines.splice(p.line, 1, ...mid);
				this.apply(newLines, { line: p.line, col: insertCol });
			}
		}
	}

	private join(count: number): void {
		let lines = this.lines();
		let p = this.cur();
		for (let i = 0; i < Math.max(1, count - 1); i++) {
			if (p.line >= lines.length - 1) break;
			this.snap();
			const cur = lines[p.line];
			const next = (lines[p.line + 1] ?? "").replace(/^\s+/, "");
			const joinCol = cur.length;
			const newLines = [...lines];
			newLines[p.line] = cur + (cur.length && next ? " " : "") + next;
			newLines.splice(p.line + 1, 1);
			lines = newLines;
			this.apply(newLines, { line: p.line, col: joinCol });
			p = this.cur();
		}
	}

	// ------------------------------------------------------------------
	// Input handling
	// ------------------------------------------------------------------

	override handleInput(data: string): void {
		if (matchesKey(data, "escape")) {
			if (this.mode === "insert") {
				// leave insert mode, cursor onto last char like vim
				const p = this.cur();
				const len = (this.lines()[p.line] ?? "").length;
				if (p.col > 0 && p.col >= len) this.int.setCursorCol(Math.max(0, len - 1));
				this.setMode("normal");
				return;
			}
			if (this.op || this.count1 !== null || this.count2 !== null || this.pendingFind || this.pendingReplace || this.pendingG) {
				this.setMode("normal"); // clear pending state
				return;
			}
			super.handleInput(data); // abort agent, etc.
			return;
		}

		// bracketed paste & multi-byte control sequences go straight through
		if (data.includes("[200~")) {
			super.handleInput(data);
			return;
		}

		if (this.mode === "insert") {
			super.handleInput(data);
			return;
		}

		if (this.int.isShowingAutocomplete()) {
			super.handleInput(data);
			return;
		}

		this.handleNormal(data);
	}

	private handleNormal(data: string): void {
		// pending replace char (r)
		if (this.pendingReplace) {
			this.pendingReplace = false;
			const repCount = this.effCount();
			this.count1 = null;
			this.count2 = null;
			if (data.length === 1 && data.charCodeAt(0) >= 32) {
				const lines = this.lines();
				const p = this.cur();
				const n = Math.min(repCount, (lines[p.line] ?? "").length - p.col);
				if (n > 0) {
					this.snap();
					const newLines = [...lines];
					newLines[p.line] = (lines[p.line] ?? "").slice(0, p.col) + data.repeat(n) + (lines[p.line] ?? "").slice(p.col + n);
					this.apply(newLines, { line: p.line, col: p.col + n - 1 });
				}
			}
			return;
		}

		// pending find char (f/t/F/T)
		if (this.pendingFind) {
			const { type, count } = this.pendingFind;
			this.pendingFind = null;
			if (data.length === 1 && data.charCodeAt(0) >= 32) {
				this.lastFind = { type, char: data, count };
				const target = this.findCharInLine(this.cur(), type, data, count);
				if (target) {
					// f/t/F stop on the char (inclusive); T stops before it (exclusive)
					const res: MotionResult = { pos: target, inclusive: type !== "T" };
					if (this.op) {
						const op = this.op;
						this.op = null;
						this.applyOperatorForMotion(op, res);
					} else {
						this.finishMotion(res);
					}
				}
			}
			return;
		}

		// pending g (gg)
		if (this.pendingG) {
			this.pendingG = false;
			if (data === "g") {
				// gg: jump to the top of the transcript, like vim's gg on a viewport
				const tui = this.int.tui;
				if (typeof tui?.scrollToTop === "function") tui.scrollToTop();
				return;
			}
			this.count1 = null;
			this.count2 = null;
			// fall through: treat as unknown key
		}

		// counts (0 is a motion when no count is active) — single digits only, so
		// the mode label's reserved width (" 9d9·PENDING ") always fits
		if (data >= "1" && data <= "9") {
			const n = Number(data);
			if (this.op) this.count2 = Math.min(9, (this.count2 ?? 0) * 10 + n);
			else this.count1 = Math.min(9, (this.count1 ?? 0) * 10 + n);
			return;
		}
		if (data === "0" && (this.count1 !== null || this.count2 !== null)) {
			if (this.op) this.count2 = Math.min(9, (this.count2 ?? 0) * 10);
			else this.count1 = Math.min(9, (this.count1 ?? 0) * 10);
			return;
		}

		// operator-pending: expect a motion or the doubled operator
		if (this.op) {
			const op = this.op;
			const total = this.effCount();
			const hasCount = this.count1 !== null || this.count2 !== null;
			if (data === op) {
				const line = this.cur().line;
				const to = Math.min(this.lines().length - 1, line + total - 1);
				this.op = null;
				this.count1 = null;
				this.count2 = null;
				this.applyOperator(op, { line, col: 0 }, { line: to, col: 0 }, true);
				return;
			}
			// operator + find-char: keep the operator pending until the char arrives
			if (data === "f" || data === "t" || data === "F" || data === "T") {
				this.pendingFind = { type: data, count: total };
				this.count1 = null;
				this.count2 = null;
				return;
			}
			this.op = null;
			const res = this.resolveMotion(data, total, hasCount);
			this.count1 = null;
			this.count2 = null;
			if (res) {
				this.applyOperatorForMotion(op, res);
			}
			return;
		}

		switch (data) {
			// --- operator start ---
			case "d":
			case "c":
			case "y":
				this.op = data;
				this.count2 = null;
				return;

			// --- pending-char motions ---
			case "f":
			case "t":
			case "F":
			case "T":
				this.pendingFind = { type: data, count: this.effCount() };
				this.count1 = null;
				return;

			case "g":
				this.pendingG = true;
				return;

			// G: jump to the end of the transcript (scroll viewport to bottom)
			case "G": {
				const tui = this.int.tui;
				if (typeof tui?.scrollToBottom === "function") tui.scrollToBottom();
				return;
			}

			case "r":
				this.pendingReplace = true;
				return;

			// --- insert entry ---
			case "i":
				this.enterInsert();
				return;
			case "/": {
				// normal mode has no use for "/": enter insert mode and type it,
				// so pi slash commands like /reload can be typed immediately
				this.enterInsert();
				super.handleInput("/");
				return;
			}
			case "I": {
				this.int.setCursorCol(this.firstNonBlank(this.cur().line));
				this.enterInsert();
				return;
			}
			case "a": {
				const p = this.cur();
				const len = (this.lines()[p.line] ?? "").length;
				if (len > 0 && p.col < len) this.int.setCursorCol(p.col + 1);
				this.enterInsert();
				return;
			}
			case "A": {
				const p = this.cur();
				this.int.setCursorCol((this.lines()[p.line] ?? "").length);
				this.enterInsert();
				return;
			}
			case "o":
			case "O": {
				const lines = this.lines();
				const p = this.cur();
				const at = data === "o" ? p.line + 1 : p.line;
				const indent = ((lines[p.line] ?? "").match(/^\s*/))?.[0] ?? "";
				this.snap();
				const newLines = [...lines.slice(0, at), indent, ...lines.slice(at)];
				this.apply(newLines, { line: at, col: indent.length });
				this.enterInsert();
				return;
			}

			// --- simple edits ---
			case "x": {
				const lines = this.lines();
				const p = this.cur();
				const len = (lines[p.line] ?? "").length;
				if (len === 0) return;
				const n = Math.min(this.effCount(), len - p.col);
				if (n <= 0) return;
				this.register = { text: (lines[p.line] ?? "").slice(p.col, p.col + n), linewise: false };
				this.deleteCharwise({ line: p.line, col: p.col }, { line: p.line, col: p.col + n });
				this.int.setCursorCol(Math.min(p.col, Math.max(0, (this.lines()[p.line] ?? "").length - 1)));
				return;
			}
			case "X": {
				const p = this.cur();
				if (p.col === 0) return;
				const n = Math.min(this.effCount(), p.col);
				this.register = { text: (this.lines()[p.line] ?? "").slice(p.col - n, p.col), linewise: false };
				this.deleteCharwise({ line: p.line, col: p.col - n }, { line: p.line, col: p.col });
				return;
			}
			case "s": {
				const p = this.cur();
				const len = (this.lines()[p.line] ?? "").length;
				const n = Math.min(this.effCount(), Math.max(0, len - p.col));
				if (n > 0) {
					this.register = { text: (this.lines()[p.line] ?? "").slice(p.col, p.col + n), linewise: false };
					this.deleteCharwise({ line: p.line, col: p.col }, { line: p.line, col: p.col + n });
				}
				this.enterInsert();
				return;
			}
			case "S": {
				const line = this.cur().line;
				const to = Math.min(this.lines().length - 1, line + this.effCount() - 1);
				this.applyOperator("c", { line, col: 0 }, { line: to, col: (this.lines()[to] ?? "").length }, false);
				return;
			}
			case "D": {
				const p = this.cur();
				this.register = { text: (this.lines()[p.line] ?? "").slice(p.col), linewise: false };
				this.deleteCharwise({ line: p.line, col: p.col }, { line: p.line, col: (this.lines()[p.line] ?? "").length });
				this.int.setCursorCol(Math.max(0, p.col - 1));
				return;
			}
			case "C": {
				const p = this.cur();
				this.register = { text: (this.lines()[p.line] ?? "").slice(p.col), linewise: false };
				this.deleteCharwise({ line: p.line, col: p.col }, { line: p.line, col: (this.lines()[p.line] ?? "").length });
				this.enterInsert();
				return;
			}
			case "J":
				this.join(this.effCount());
				return;
			case "~": {
				const lines = this.lines();
				const p = this.cur();
				const text = lines[p.line] ?? "";
				const n = Math.min(this.effCount(), text.length - p.col);
				if (n <= 0) return;
				let out = "";
				for (let i = 0; i < n; i++) {
					const ch = text[p.col + i]!;
					out += ch === ch.toLowerCase() ? ch.toUpperCase() : ch.toLowerCase();
				}
				this.snap();
				const newLines = [...lines];
				newLines[p.line] = text.slice(0, p.col) + out + text.slice(p.col + n);
				this.apply(newLines, { line: p.line, col: Math.min(p.col + n, newLines[p.line]!.length - 1) });
				return;
			}

			// --- paste ---
			case "p":
				this.paste(true, this.effCount());
				return;
			case "P":
				this.paste(false, this.effCount());
				return;

			// --- undo / redo ---
			case "u": {
				if (this.int.undoStack.length === 0) return;
				this.redoStack.push(this.snapNow());
				this.int.undo();
				return;
			}
			case "ctrl+r": {
				const next = this.redoStack.pop();
				if (!next) return;
				this.restore(next);
				return;
			}

			default:
				break;
		}

		// --- pure motions (j/k fall through to the base editor for history
		// browsing when they cannot move, matching the stock editor behavior) ---
		const orig = data;
		if (matchesKey(data, "up")) data = "k";
		else if (matchesKey(data, "down")) data = "j";
		else if (matchesKey(data, "left")) data = "h";
		else if (matchesKey(data, "right")) data = "l";

		const total = this.effCount();
		const hasCount = this.count1 !== null || this.count2 !== null;
		this.count1 = null;
		this.count2 = null;
		const res = this.resolveMotion(data, total, hasCount);
		if (res) {
			const startLine = this.cur().line;
			this.finishMotion(res);
			if ((data === "j" || data === "k") && res.pos.line === startLine) {
				// Cannot move further within the input: scroll the transcript one
				// line (per count), exactly like the arrow keys do via their
				// tui.altScreen.lineUp/lineDown bindings.
				const tui = this.int.tui;
				if (typeof tui?.scrollBy === "function") tui.scrollBy(data === "j" ? total : -total);
			}
			return;
		}

		// not a vim key: printable chars are ignored in normal mode (never insert
		// text); control keys are passed to the base editor / app
		if (data.length === 1 && data.charCodeAt(0) >= 32) return;
		super.handleInput(data);
	}

	/** Resolve a motion key, handling f/t/F/T repeats via ; and , . */
	private resolveMotion(data: string, count: number, hasCount = false): MotionResult | null {
		if (data === ";") {
			if (!this.lastFind) return null;
			const target = this.findCharInLine(this.cur(), this.lastFind.type, this.lastFind.char, this.lastFind.count);
			return target ? { pos: target, inclusive: true } : null;
		}
		if (data === ",") {
			if (!this.lastFind) return null;
			const flip: Record<string, "f" | "t" | "F" | "T"> = { f: "F", F: "f", t: "T", T: "t" };
			const type = flip[this.lastFind.type];
			const target = this.findCharInLine(this.cur(), type, this.lastFind.char, this.lastFind.count);
			return target ? { pos: target, inclusive: true } : null;
		}
		if (data === "%") {
			const target = this.matchBracket(this.cur());
			return target ? { pos: target, inclusive: true } : null;
		}
		return this.motion(data, count, hasCount);
	}

	/** Apply an operator to the range described by a motion result. */
	private applyOperatorForMotion(op: "d" | "c" | "y", res: MotionResult): void {
		const cur = this.cur();
		let start = cur;
		let end = res.pos;
		if (start.line > end.line || (start.line === end.line && start.col > end.col)) {
			[start, end] = [end, start];
		}
		if (res.linewise) {
			this.applyOperator(op, { line: start.line, col: 0 }, { line: end.line, col: (this.lines()[end.line] ?? "").length }, true);
			return;
		}
		if (res.inclusive) end = { line: end.line, col: end.col + 1 };
		// vim special case: cw behaves like de (don't eat trailing whitespace)
		this.applyOperator(op, start, end, false);
	}

	/** Move the cursor for a pure motion, with normal-mode clamping. */
	private finishMotion(res: MotionResult): void {
		const p = res.pos;
		if (res.linewise) {
			this.int.state.cursorLine = p.line;
			const clamped = this.normalClamp({ line: p.line, col: this.cur().col });
			this.int.setCursorCol(clamped.col);
		} else {
			this.int.state.cursorLine = p.line;
			this.int.setCursorCol(p.col);
		}
	}

	private effCount(): number {
		return (this.count1 ?? 1) * (this.count2 ?? 1);
	}

	// ------------------------------------------------------------------
	// Mode indicator (start of the prompt line)
	// ------------------------------------------------------------------

	/** Mode label: NORMAL / INSERT / pending operator ("2d3").
	 * All states are padded to a constant width (NORMAL/INSERT are the widest
	 * at 8 cols, " 9d9 " fits inside), so the prompt text never shifts.
	 * Style: bold black text on a colored background (theme accent colors). */
	private modeLabel(): string {
		const LABEL_WIDTH = " NORMAL ".length;
		const padTo = (s: string): string => s + " ".repeat(Math.max(0, LABEL_WIDTH - visibleWidth(s)));
		const theme = fullThemeRef?.ui?.theme as
			| {
					style?: (text: string, options: Record<string, unknown>) => string;
					colors?: Record<string, unknown>;
				}
			| undefined;
		const styled = (text: string, token: string): string => {
			const bgColor = theme?.colors?.[token];
			if (typeof theme?.style === "function" && bgColor) {
				// Black as a concrete rgb Color (strings would be theme token names);
				// bg = a theme color token so the label actually has color.
				return theme.style(text, { fg: { kind: "rgb", r: 0, g: 0, b: 0 }, bg: bgColor, bold: true });
			}
			return text;
		};
		if (this.mode === "insert") {
			return padTo(styled(" INSERT ", "success"));
		}
		if (this.op) {
			// vim showcmd style: "2d3" (count typed before op, op, count after op)
			const cnt =
				(this.count1 === null ? "" : String(this.count1)) +
				this.op +
				(this.count2 === null ? "" : String(this.count2));
			return padTo(styled(` ${cnt} `, "warning"));
		}
		return padTo(styled(" NORMAL ", "accent"));
	}

	/** Prepend the mode label + one plain space to the first prompt line, before the text/cursor. */
	protected override render(width: number): string[] {
		const lines = super.render(width);
		const label = this.modeLabel();
		// lines[0] is the top border; lines[1] is the first prompt/content line.
		// Prepending is cursor-safe: the TUI locates the cursor by searching for
		// CURSOR_MARKER inside the rendered line, so the label just shifts it.
		if (lines.length > 2) {
			lines[1] = truncateToWidth(label + " " + lines[1], width, "");
		}
		return lines;
	}
}

/** Live ExtensionContext, stashed on session_start so the editor can reach
 * ctx.ui.theme (the factory only receives the reduced editor theme). */
let fullThemeRef: any = undefined;

export default function (pi: ExtensionAPI) {
	pi.on("session_start", (_event, ctx) => {
		fullThemeRef = ctx;
		ctx.ui.setEditorComponent((tui, theme, kb) => new VimEditor(tui, theme, kb));
	});
	pi.on("session_end", () => {
		// restore a sane cursor shape if pi exits while in insert mode
		process.stdout.write("\x1b[2 q");
	});
}
