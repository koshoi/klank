// show-diagram.ts - pi extension: agent tool that opens a mermaid diagram
// in the user's browser via klankd.
//
// The tool POSTs a `show_diagram` message to the in-container bridge
// (klank/bridge.ts), which queues it in its outbox. klankd on the host
// consumes the outbox over SSE (klank/cmd/klankd/consumer.go), encodes
// the mermaid source into a mermaid.live URL, opens it in the user's
// browser, and POSTs the outcome back to the bridge (/inbox). The tool
// polls for that result and returns the URL (or the failure) to the
// model.
//
// Pipeline: this tool -> bridge /outbox -> SSE /events -> klankd handler
// -> bridge /inbox -> this tool's result text.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const PORT = process.env.KLANK_PORT || "8089";
const BASE = `http://127.0.0.1:${PORT}`;
const POLL_MS = 500;
const POLL_TIMEOUT_MS = 10_000;

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

type DiagramDetails = { ok?: boolean; url?: string; error?: string };

const text = (t: string, details: DiagramDetails = {}) => ({ content: [{ type: "text" as const, text: t }], details });

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "show_diagram",
		label: "Show Diagram",
		description:
			"Show a mermaid diagram to the user: encodes the mermaid source into a mermaid.live URL and opens it in the user's browser (via klankd on the host). Use when a visual diagram helps the user understand structure — architecture, data flow, state machines, sequence diagrams. The user sees the rendered diagram; you do not. The result tells you whether it opened.",
		parameters: Type.Object({
			title: Type.Optional(
				Type.String({ description: "Optional short title for the diagram (surfaced to the user by the host)" }),
			),
			mermaid: Type.String({
				description:
					"Mermaid diagram source, e.g. 'graph TD; A[Server] --> B[Database]' or a sequenceDiagram. Must be non-empty.",
			}),
		}),

		async execute(_toolCallId, params, signal) {
			signal?.throwIfAborted();
			const mermaid = typeof params.mermaid === "string" ? params.mermaid.trim() : "";
			if (!mermaid) {
				return text("Error: mermaid source is empty — provide the diagram source.");
			}

			// 1. enqueue on the bridge
			let id: string;
			try {
				const res = await fetch(`${BASE}/outbox`, {
					method: "POST",
					headers: { "content-type": "application/json" },
					body: JSON.stringify({ type: "show_diagram", data: { title: params.title, mermaid } }),
					signal,
				});
				if (!res.ok) {
					const body = await res.text();
					return text(`klank bridge rejected the message (HTTP ${res.status}): ${body}`, { error: body });
				}
				const queued = (await res.json()) as { id?: string };
				if (!queued.id) {
					return text(`klank bridge returned no message id: ${JSON.stringify(queued)}`, { error: "no message id" });
				}
				id = queued.id;
			} catch (err) {
				if (signal?.aborted) throw err;
				return text(
					`Could not reach the klank bridge at ${BASE} (${err instanceof Error ? err.message : String(err)}). The show_diagram tool only works inside a klank container.`,
					{ error: "bridge unreachable" },
				);
			}

			// 2. poll for the host-delivered result (klankd handles + acks)
			const deadline = Date.now() + POLL_TIMEOUT_MS;
			while (Date.now() < deadline) {
				signal?.throwIfAborted();
				await sleep(POLL_MS);
				let res: Response;
				try {
					res = await fetch(`${BASE}/outbox/${id}/result`, { signal });
				} catch (err) {
					if (signal?.aborted) throw err;
					return text(`Lost contact with the klank bridge while waiting for the result: ${err instanceof Error ? err.message : String(err)}`);
				}
				if (res.status === 404) continue; // not handled yet
				if (!res.ok) {
					return text(`klank bridge error while fetching the result (HTTP ${res.status}).`, { error: `HTTP ${res.status}` });
				}
				const result = (await res.json()) as { ok?: boolean; data?: { url?: string; error?: string } };
				const url = result.data?.url;
				if (result.ok) {
					return text(
						`Diagram opened in the user's browser: ${url}\n\nTell the user the diagram is open, and point at the URL only if they cannot find the tab.`,
						{ ok: true, url },
					);
				}
				return text(
					`Failed to open the diagram in the user's browser: ${result.data?.error ?? "unknown error"}\nThe mermaid.live URL is still valid — share it directly: ${url ?? "(unavailable)"}`,
					{ ok: false, url, error: result.data?.error },
				);
			}
			return text(
				`The diagram was queued (id ${id}) but no result arrived within ${POLL_TIMEOUT_MS / 1000}s. klankd may not be running on the host, or is slow to pick it up.`,
				{ error: "result timeout" },
			);
		},
	});
}
