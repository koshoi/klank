// user-borders.ts - pi extension: draw ASCII rules around user messages.
//
// Wraps every user message between horizontal rules with a YOU label, so
// prompts stay visually distinct from the model's output. Borders are
// colorized via ANSI codes; color is skipped when the terminal likely
// can't display it (NO_COLOR, TERM=dumb, or a non-TTY stdout).
//
// Uses registerMarkdownTransformer, which is display-only: the session
// transcript and model context keep the original text.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// SGR sequences: rules use cyan, the YOU label is bold cyan.
const CYAN = "\x1b[36m";
const BOLD_CYAN = "\x1b[1;36m";
const RESET = "\x1b[0m";

function colorSupported(): boolean {
	if (process.env.NO_COLOR) return false;
	if (process.env.FORCE_COLOR) return true;
	if (!process.stdout.isTTY) return false;
	const term = process.env.TERM ?? "";
	return term !== "dumb" && term !== "";
}

export default function (pi: ExtensionAPI) {
	const useColor = colorSupported();

	pi.registerMarkdownTransformer((markdown, { messageType, isStreaming, availableWidth }) => {
		// only user messages; streaming is assistant-only, thinking is separate
		if (messageType !== "user" || isStreaming) return markdown;

		const width = Math.max(availableWidth, 20);
		const label = " YOU ";
		const inner = width - label.length - 2;
		const left = Math.max((inner >> 1) + (inner & 1), 1);
		const right = Math.max(inner - left, 0);

		if (!useColor) {
			return `[${"=".repeat(left)}${label}${"=".repeat(right)}]\n\n${markdown.trimEnd()}\n\n${"=".repeat(width)}`;
		}

		const top =
			CYAN + "[" + "=".repeat(left) + BOLD_CYAN + label + RESET + CYAN + "=".repeat(right) + "]" + RESET;
		const bottom = CYAN + "=".repeat(width) + RESET;

		// blank lines around the message prevent setext-heading interpretation
		// (a `===` line directly under text would render it as an H1)
		return `${top}\n\n${markdown.trimEnd()}\n\n${bottom}`;
	});
}
