// bridge.ts - pi extension: external message inbox for klank.
//
// Watches the spool directory /tmp/pi-inbox inside the container. Any file
// named msg-<id> dropped there is read, removed, and injected into the
// running pi session as if typed by the user (pi.sendUserMessage). It shows
// up in the TUI transcript like any other user message.
//
// Receipts are written next to the message file:
//   msg-<id>.sent  - message delivered to the agent (started or queued)
//   msg-<id>.done  - agent settled after the message (turn finished)
//   msg-<id>.error - delivery failed (note contains the error)
//
// `klank send` writes messages (to a tmp file, then atomically renames it)
// and with --wait polls for the .done receipt.

import * as fs from "node:fs";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const INBOX = "/tmp/pi-inbox";

export default function (pi: ExtensionAPI) {
	let watcher: fs.FSWatcher | null = null;
	let busy = false; // tracked via agent_start / agent_settled
	const inFlight = new Set<string>(); // message ids awaiting agent settle

	const receipt = (id: string, kind: string, note = "") => {
		try {
			fs.writeFileSync(path.join(INBOX, `${id}.${kind}`), note);
		} catch (err) {
			console.error(`[klank-bridge] cannot write ${kind} receipt for ${id}: ${err}`);
		}
	};

	const drain = () => {
		let files: string[];
		try {
			files = fs.readdirSync(INBOX);
		} catch {
			return; // inbox missing (e.g. cleaned up); nothing to do
		}
		for (const f of files) {
			// message files are msg-<id> (no dot); receipts contain a dot
			if (!f.startsWith("msg-") || f.includes(".")) continue;
			const p = path.join(INBOX, f);
			let text: string;
			try {
				text = fs.readFileSync(p, "utf8");
				fs.unlinkSync(p);
			} catch {
				continue; // vanished mid-read; a later event retries
			}
			if (!text.trim()) continue;
			try {
				if (busy) {
					// mid-run: queue as steering (delivered after the current
					// tool calls finish)
					pi.sendUserMessage(text, { deliverAs: "steer" });
				} else {
					pi.sendUserMessage(text);
				}
				inFlight.add(f);
				receipt(f, "sent", busy ? "queued" : "started");
				console.error(`[klank-bridge] injected ${f} (${busy ? "steer" : "prompt"})`);
			} catch (err) {
				receipt(f, "error", String(err));
				console.error(`[klank-bridge] failed to inject ${f}: ${err}`);
			}
		}
	};

	pi.on("agent_start", () => {
		busy = true;
	});

	pi.on("agent_settled", () => {
		busy = false;
		if (inFlight.size === 0) return;
		for (const id of inFlight) receipt(id, "done");
		inFlight.clear();
	});

	pi.on("session_start", () => {
		try {
			fs.mkdirSync(INBOX, { recursive: true, mode: 0o777 });
		} catch (err) {
			console.error(`[klank-bridge] cannot create ${INBOX}: ${err}`);
			return;
		}
		// session_start can fire again after /reload or /new; avoid leaks
		watcher?.close();
		watcher = fs.watch(INBOX, () => drain());
		drain(); // pick up anything that arrived before startup
	});

	pi.on("session_shutdown", () => {
		watcher?.close();
		watcher = null;
	});
}
