// bridge.ts - pi extension: external control server for klank.
//
// Runs a small HTTP server inside the container (port from KLANK_PORT,
// default 8089). `klank new` publishes that port on the host's loopback, so
// the klank CLI (or plain curl) can talk to the running pi session:
//
//   GET  /          discovery API: service info + endpoint listing
//   GET  /health     liveness probe -> {"ok":true}
//   GET  /status     agent state: busy/idle, model, session, context usage
//   POST /message    body = message text, injected as a user message
//                    (?wait=1 holds the response until the agent settles)
//
// All responses are JSON. Delivery follows the same rules as typing in the
// TUI: when the agent is idle the message starts a new run, mid-run it is
// queued as steering (delivered after the current tool calls finish).
//
// For compatibility with older klank binaries the legacy file spool
// (/tmp/pi-inbox) is still watched: files named msg-<id> are read, removed
// and injected exactly like POST /message; .sent/.done/.error receipt files
// are written next to them.

import * as fs from "node:fs";
import * as http from "node:http";
import * as path from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

const INBOX = "/tmp/pi-inbox";
const PORT = Number.parseInt(process.env.KLANK_PORT || "8089", 10);
const MAX_BODY = 1024 * 1024; // 1 MiB message cap

export default function (pi: ExtensionAPI) {
	// subagent extension spawns child `pi` processes which load this extension
	// too; only the top-level session may expose the control server / consume
	// the inbox, otherwise N watchers race to inject the same message
	if (process.env.KLANK_SUBAGENT === "1") {
		console.error("[klank-bridge] subagent process; control server disabled");
		return;
	}

	let watcher: fs.FSWatcher | null = null;
	let busy = false; // tracked via agent_start / agent_settled
	let lastCtx: ExtensionContext | null = null; // refreshed on every event
	const startedAt = Date.now();
	const inFlight = new Set<string>(); // spool message ids awaiting agent settle
	const waiters: Array<() => void> = []; // POST /message?wait=1 responders

	const receipt = (id: string, kind: string, note = "") => {
		try {
			fs.writeFileSync(path.join(INBOX, `${id}.${kind}`), note);
		} catch (err) {
			console.error(`[klank-bridge] cannot write ${kind} receipt for ${id}: ${err}`);
		}
	};

	// inject sends a message into the running session and returns whether it
	// started a new run (false = queued as steering behind the current one)
	const inject = (text: string): boolean => {
		if (busy) {
			pi.sendUserMessage(text, { deliverAs: "steer" });
			return false;
		}
		pi.sendUserMessage(text);
		return true;
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
				const started = inject(text);
				inFlight.add(f);
				receipt(f, "sent", started ? "started" : "queued");
				console.error(`[klank-bridge] injected ${f} (${started ? "prompt" : "steer"})`);
			} catch (err) {
				receipt(f, "error", String(err));
				console.error(`[klank-bridge] failed to inject ${f}: ${err}`);
			}
		}
	};

	const remember = (ctx: ExtensionContext) => {
		lastCtx = ctx;
	};

	pi.on("agent_start", (ctx: ExtensionContext) => {
		remember(ctx);
		busy = true;
	});

	pi.on("agent_settled", (ctx: ExtensionContext) => {
		remember(ctx);
		busy = false;
		if (inFlight.size === 0 && waiters.length === 0) return;
		for (const id of inFlight) receipt(id, "done");
		inFlight.clear();
		const pending = waiters.splice(0);
		for (const w of pending) w();
	});

	pi.on("session_start", (ctx: ExtensionContext) => {
		remember(ctx);
		try {
			fs.mkdirSync(INBOX, { recursive: true, mode: 0o777 });
		} catch (err) {
			console.error(`[klank-bridge] cannot create ${INBOX}: ${err}`);
		}
		// session_start can fire again after /reload or /new; avoid leaks
		watcher?.close();
		try {
			watcher = fs.watch(INBOX, () => drain());
		} catch (err) {
			console.error(`[klank-bridge] cannot watch ${INBOX}: ${err}`);
			watcher = null;
		}
		drain(); // pick up anything that arrived before startup
	});

	pi.on("session_shutdown", () => {
		watcher?.close();
		watcher = null;
		server.close();
	});

	// ---- HTTP control server ----

	const json = (res: http.ServerResponse, code: number, body: unknown) => {
		const data = JSON.stringify(body);
		res.writeHead(code, { "content-type": "application/json" });
		res.end(data);
	};

	const readBody = (req: http.IncomingMessage): Promise<string> =>
		new Promise((resolve, reject) => {
			let size = 0;
			const chunks: Buffer[] = [];
			req.on("data", (c: Buffer) => {
				size += c.length;
				if (size > MAX_BODY) {
					reject(new Error("body too large"));
					req.destroy();
					return;
				}
				chunks.push(c);
			});
			req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
			req.on("error", reject);
		});

	const statusPayload = () => {
		const ctx = lastCtx;
		let model: unknown = null;
		if (ctx?.model) {
			const m = ctx.model as unknown as Record<string, unknown>;
			model = { id: m.id ?? null, provider: m.provider ?? null, name: m.name ?? null };
		}
		// event contexts do not always carry the optional action helpers;
		// only call them when present
		const call = (fn: unknown, ...args: unknown[]) =>
			typeof fn === "function" ? (fn as (...a: unknown[]) => unknown)(...args) : null;
		const sm = ctx?.sessionManager as unknown as Record<string, unknown> | undefined;
		return {
			pid: process.pid,
			uptimeSec: Math.round((Date.now() - startedAt) / 1000),
			busy,
			idle: call(ctx?.isIdle) ?? !busy,
			pending: call(ctx?.hasPendingMessages),
			model,
			thinkingLevel: ctx?.thinkingLevel ?? null,
			cwd: ctx?.cwd ?? null,
			sessionFile: call(sm?.getSessionFile),
			sessionName: call(sm?.getSessionName),
			contextUsage: call(ctx?.getContextUsage),
		};
	};

	// discoveryPayload describes the control server: service identity plus
	// every endpoint it serves. Clients (and humans) can point at "/" to
	// learn what the bridge offers without out-of-band documentation.
	const discoveryPayload = () => ({
		service: "klank-bridge",
		version: 1,
		pid: process.pid,
		endpoints: [
			{ method: "GET", path: "/", description: "this discovery document" },
			{ method: "GET", path: "/health", description: `liveness probe -> ${JSON.stringify({ ok: true })}` },
			{ method: "GET", path: "/status", description: "agent state: busy/idle, model, session, context usage" },
			{ method: "POST", path: "/message", description: "inject a user message (body = text; ?wait=1 long-polls until the agent settles)" },
		],
	});

	const server = http.createServer(async (req, res) => {
		const url = new URL(req.url || "/", "http://klank");
		try {
			if (req.method === "GET" && (url.pathname === "/" || url.pathname === "")) {
				json(res, 200, discoveryPayload());
				return;
			}
			if (req.method === "GET" && url.pathname === "/health") {
				json(res, 200, { ok: true });
				return;
			}
			if (req.method === "GET" && url.pathname === "/status") {
				json(res, 200, statusPayload());
				return;
			}
			if (req.method === "POST" && url.pathname === "/message") {
				const text = (await readBody(req)).trim();
				if (!text) {
					json(res, 400, { error: "empty message" });
					return;
				}
				let started: boolean;
				try {
					started = inject(text);
				} catch (err) {
					json(res, 500, { error: String(err) });
					return;
				}
				console.error(`[klank-bridge] http message (${started ? "prompt" : "steer"})`);
				if (url.searchParams.get("wait") !== "1") {
					json(res, 200, { status: started ? "started" : "queued" });
					return;
				}
				// long-poll: hold the response until the agent settles (or is
				// disconnected); klank's --timeout maps to its client timeout
				await new Promise<void>((resolve) => waiters.push(resolve));
				json(res, 200, { status: "done" });
				return;
			}
			json(res, 404, { error: `no such endpoint: ${req.method} ${url.pathname}` });
		} catch (err) {
			json(res, 500, { error: String(err) });
		}
	});

	server.on("clientError", (_err, socket) => {
		socket.end("HTTP/1.1 400 Bad Request\r\n\r\n");
	});

	// listen on all interfaces inside the container; `docker run -p
	// 127.0.0.1:<port>:<port>` keeps it loopback-only on the host
	server.listen(PORT, () => {
		console.error(`[klank-bridge] control server listening on port ${PORT}`);
	});
	server.on("error", (err) => {
		console.error(`[klank-bridge] control server failed: ${err}`);
	});
}
