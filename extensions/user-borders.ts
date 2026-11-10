// user-borders.ts - pi extension: draw ASCII rules around user messages.
//
// For primitive terminals where theme colors are unreliable: wraps every
// user message between horizontal rules with a YOU label, so prompts stay
// visually distinct from the model's output regardless of color support.
//
// Uses registerMarkdownTransformer, which is display-only: the session
// transcript and model context keep the original text.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
	pi.registerMarkdownTransformer((markdown, { messageType, isStreaming, availableWidth }) => {
		// only user messages; streaming is assistant-only, thinking is separate
		if (messageType !== "user" || isStreaming) return markdown;

		const width = Math.max(availableWidth, 20);
		const label = " YOU ";
		const inner = width - label.length - 2;
		const left = Math.max((inner >> 1) + (inner & 1), 1);
		const right = Math.max(inner - left, 0);
		const top = "[" + "=".repeat(left) + label + "=".repeat(right) + "]";
		const bottom = "=".repeat(width);

		// blank lines around the message prevent setext-heading interpretation
		// (a `===` line directly under text would render it as an H1)
		return `${top}\n\n${markdown.trimEnd()}\n\n${bottom}`;
	});
}
