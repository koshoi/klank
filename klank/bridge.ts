// bridge.ts - pi extension: external control server for klank.
//
// Runs a small HTTP server inside the container (port from KLANK_PORT,
// default 8089). `klank new` publishes that port on the host's loopback, so
// the klank CLI (or plain curl) can talk to the running pi session:
//
//   GET  /          discovery API: service info + endpoint listing
//   GET  /health     liveness probe -> {"ok":true}
//   GET  /status     agent state: busy/idle, model, session, context usage
//   GET  /messages   recent transcript messages, indexed newest-first
//                    (?from=0&limit=1&role=user,assistant&text=1)
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

	pi.on("agent_start", (_event, ctx: ExtensionContext) => {
		remember(ctx);
		busy = true;
	});

	pi.on("agent_settled", (_event, ctx: ExtensionContext) => {
		remember(ctx);
		busy = false;
		if (inFlight.size === 0 && waiters.length === 0) return;
		for (const id of inFlight) receipt(id, "done");
		inFlight.clear();
		const pending = waiters.splice(0);
		for (const w of pending) w();
	});

	pi.on("session_start", (_event, ctx: ExtensionContext) => {
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
			{ method: "GET", path: "/messages", description: "recent transcript messages, newest-first (?from=0 offset, ?limit=1, ?role=user,assistant,toolResult, ?text=1 flattens content to text)" },
			{ method: "POST", path: "/message", description: "inject a user message (body = text; ?wait=1 long-polls until the agent settles)" },
		],
	});

	// ---- GET /messages helpers ----

	// parseCount validates a non-negative integer query param; null = invalid,
	// missing/empty = fallback (0)
	const parseCount = (raw: string | null): number | null => {
		if (raw === null || raw === "") return 0;
		if (!/^\d+$/.test(raw)) return null;
		const n = Number.parseInt(raw, 10);
		return Number.isSafeInteger(n) ? n : null;
	};

	const LIMIT_MAX = 500;
	const MESSAGE_ROLES = new Set(["user", "assistant", "toolResult"]);

	// flattenText joins text blocks of a message content (string or block array)
	const flattenText = (content: unknown): string => {
		if (typeof content === "string") return content;
		if (!Array.isArray(content)) return "";
		return content
			.filter((b) => b && typeof b === "object" && (b as AnyBlock).type === "text")
			.map((b) => (b as AnyBlock).text ?? "")
			.join("\n\n");
	};

	type AnyBlock = { type?: string; text?: string; id?: string; name?: string; arguments?: unknown };
	type AnyEntry = { type?: string; id?: string; parentId?: string | null; timestamp?: string; message?: AnyMsg };
	type AnyMsg = {
		role?: string;
		content?: unknown;
		provider?: string;
		model?: string;
		stopReason?: string;
		toolName?: string;
		isError?: boolean;
	};

	// shapeMessage projects a session entry into the wire format for /messages
	const shapeMessage = (entry: AnyEntry, raw: boolean): Record<string, unknown> => {
		const msg = entry.message ?? {};
		const role = msg.role ?? "";
		const out: Record<string, unknown> = {
			id: entry.id ?? null,
			parentId: entry.parentId ?? null,
			timestamp: entry.timestamp ?? null,
			role,
		};
		if (raw) {
			out.content = msg.content ?? [];
		} else {
			out.text = flattenText(msg.content);
		}
		if (role === "assistant") {
			if (msg.model) out.model = { id: msg.model, provider: msg.provider ?? null };
			if (msg.stopReason) out.stopReason = msg.stopReason;
			const blocks = Array.isArray(msg.content) ? (msg.content as AnyBlock[]) : [];
			out.toolCalls = blocks
				.filter((b) => b?.type === "toolCall")
				.map((b) => ({ id: b.id ?? null, name: b.name ?? null, arguments: b.arguments ?? {} }));
		} else if (role === "toolResult") {
			out.toolName = msg.toolName ?? null;
			out.isError = Boolean(msg.isError);
		}
		return out;
	};

	const messagesPayload = (url: URL): Record<string, unknown> | { error: string } => {
		const sp = url.searchParams;

		const from = parseCount(sp.get("from"));
		if (from === null) return { error: "invalid from: expected a non-negative integer" };
		const limitRaw = parseCount(sp.get("limit"));
		if (limitRaw === null) return { error: "invalid limit: expected a non-negative integer" };
		const limit = Math.min(Math.max(limitRaw, 1), LIMIT_MAX);

		const roleParam = sp.get("role");
		let roles: Set<string> | null = null;
		if (roleParam) {
			roles = new Set<string>();
			for (const r of roleParam.split(",")) {
				const role = r.trim();
				if (!role) continue; // tolerate empty items ("user,", " user")
				if (!MESSAGE_ROLES.has(role)) return { error: `invalid role: ${role} (allowed: user, assistant, toolResult)` };
				roles.add(role);
			}
			if (roles.size === 0) roles = null; // "," or whitespace-only = no filter
		}

		const textParam = sp.get("text") ?? "1";
		if (textParam !== "0" && textParam !== "1") return { error: "invalid text: expected 0 or 1" };
		const raw = textParam === "0";

		// buildContextEntries gives the active branch with compaction applied;
		// branch order is oldest -> newest
		const sm = lastCtx?.sessionManager as unknown as
			| { buildContextEntries?: () => AnyEntry[]; getBranch?: () => AnyEntry[] }
			| undefined;
		const call = (fn: unknown): AnyEntry[] => (typeof fn === "function" ? (fn as () => AnyEntry[])() : []);
		const entries = call(sm?.buildContextEntries) || call(sm?.getBranch);

		const filtered = entries.filter((e) => {
			if (!e || e.type !== "message") return false;
			const role = e.message?.role;
			if (!role || role === "system" || role === "custom") return false;
			return !roles || roles.has(role);
		});

		// total reflects the role filter (if any), not the whole transcript,
		// so clients can paginate through the filtered set with from/limit

		// newest-first indexing: reverse, then slice [from, from+limit)
		const newestFirst = filtered.reverse(); // filtered is a fresh array from .filter()
		const slice = newestFirst.slice(from, from + limit);
		return {
			total: filtered.length,
			count: slice.length,
			messages: slice.map((e) => shapeMessage(e, raw)),
		};
	};

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
			if (req.method === "GET" && url.pathname === "/messages") {
				const payload = messagesPayload(url);
				if ("error" in payload) {
					json(res, 400, payload);
					return;
				}
				json(res, 200, payload);
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
