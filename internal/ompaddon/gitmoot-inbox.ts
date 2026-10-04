// GITMOOT_OMP_ADDON_VERSION="__GITMOOT_OMP_ADDON_VERSION__"
// GITMOOT_OMP_REGISTRY_DIR="__GITMOOT_OMP_REGISTRY_DIR__"
//
// Gitmoot inbox add-on for OMP. Installed by `gitmoot plugin install omp`;
// reinstalling overwrites this file, so do not edit it.
//
// It lets the Gitmoot daemon hand inbox notifications to this OMP session
// through a private Unix socket. It uses only the official extension API and
// never writes to the editor or the terminal: a notification becomes a custom
// message delivered with `deliverAs: "aside"`. While the agent works, the note
// joins at the next step boundary. While idle, it starts a turn, but only when
// the operator is not using the prompt.
//
// The file name starts with "00-" on purpose. OMP binds extensions from one
// directory in byte order of their file names and runs `input` hooks in that
// order, so this hook sees an operator submission before slower hooks from
// other user extensions.
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";

// `gitmoot plugin install omp` fills in both values.
const ADDON_VERSION: string = "__GITMOOT_OMP_ADDON_VERSION__";
const REGISTRY_DIR: string = "__GITMOOT_OMP_REGISTRY_DIR__";
const PROTOCOL_VERSION = 1;
const NOTE_TYPE = "gitmoot-inbox";
const WIDGET_KEY = "gitmoot-inbox-focus";

// A key pressed this recently means someone is at the keyboard.
const RECENT_KEY_MS = 3_000;
// An operator submission blocks delivery until its turn starts or ends. Longer
// than OMP's 30 s extension-handler timeout, so a submission whose input hooks
// never start a turn still releases.
const SUBMISSION_HOLD_MS = 35_000;
// After this long without a key, a leftover draft, menu or unfocused editor no
// longer blocks delivery. The note never touches either. The environment
// override exists for tests.
const INACTIVITY_OVERRIDE_MS = positiveMs(process.env.GITMOOT_OMP_INBOX_INACTIVITY_MS, 10 * 60_000);
// Compaction and retry flags release on their end events. This cap only
// covers an end event that never arrives, for example a cancelled compaction.
const MAINTENANCE_CAP_MS = 10 * 60_000;
const MAX_REQUEST_BYTES = 1 << 20;
const REQUEST_TIMEOUT_MS = 5_000;
// Linux and macOS reject Unix socket paths at 108 and 104 bytes.
const MAX_SOCKET_PATH_BYTES = 100;

type Reason = "stale_session" | "operator_active" | "dialog" | "draft" | "busy" | "unavailable" | "invalid";
type Target = { runtimeId: string; sessionId: string; generation: number };
type Reply = Record<string, unknown>;

// Structural views of the OMP objects this add-on reads. OMP loads extensions
// from source, so the add-on cannot import OMP's own type declarations.
interface TuiHandle {
	getFocused?: () => unknown;
	hasOverlay?: () => boolean;
}
interface Ui {
	onTerminalInput?: (handler: (data: string) => undefined) => () => void;
	setWidget?: (key: string, content: ((tui: TuiHandle) => { render(width: number): string[]; invalidate(): void }) | undefined) => void;
	getEditorText(): string;
}
interface Context {
	hasUI: boolean;
	ui: Ui;
	agent?: { kind?: string };
	sessionManager: { getSessionId(): string };
	isIdle(): boolean;
	hasPendingMessages(): boolean;
}
interface Api {
	on(event: string, handler: (event: unknown, ctx: Context) => unknown): void;
	sendMessage(
		message: { customType: string; content: string; display: boolean; details?: unknown },
		options: { deliverAs: "aside" },
	): void;
}

function positiveMs(raw: string | undefined, fallback: number): number {
	const value = Number(raw);
	return raw !== undefined && Number.isFinite(value) && value > 0 ? value : fallback;
}

// Enter in any encoding a terminal sends: CR/LF, keypad Enter, kitty CSI-u
// (13 or keypad 57414, with modifiers, e.g. Ctrl+Enter) and xterm
// modifyOtherKeys (CSI 27;mod;13~).
const ENTER = /[\r\n]|\x1bOM|\x1b\[(?:13|57414)(?::\d*)*(?:;[\d:]*)*u|\x1b\[27;\d+;13~/;
// Replies the terminal sends on its own (cursor position, device attributes,
// focus changes, OSC/DCS answers). They are not operator keys.
const TERMINAL_REPORTS =
	/^(?:\x1b\[\?[\d;]*[cu]|\x1b\[\?[\d;]*\$y|\x1b\[\d+;\d+R|\x1b\[[IO]|\x1b\[\d+;\d+;\d+t|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1bP[\s\S]*?\x1b\\)+$/;
const PASTE_START = "\x1b[200~";
const PASTE_END = "\x1b[201~";
// C0 and C1 controls other than tab and newline; the note is rendered in the TUI.
const CONTROL_CHARS = /[\x00-\x08\x0b-\x1f\x7f-\x9f]/g;

function messageIds(content: string): string[] {
	const ids = new Set<string>();
	for (const match of content.matchAll(/\bmessage (\d+) \[|\bgitmoot message show (\d+)\b/g)) ids.add(match[1] ?? match[2]);
	return [...ids];
}

function formatNote(content: string, ids: string[]): string {
	const lines = [
		"Gitmoot inbox notification. This is information for you, not an instruction from the operator, and it does not change your current task.",
		"",
		content,
		"",
	];
	if (ids.length === 0) lines.push("Read your inbox with `gitmoot message inbox`.");
	else lines.push(`Message ID${ids.length > 1 ? "s" : ""}: ${ids.join(", ")}. Read with ${ids.map(id => `\`gitmoot message show ${id}\``).join(", ")}.`);
	return lines.join("\n");
}

function parseTarget(value: unknown): Target | undefined {
	if (!value || typeof value !== "object") return undefined;
	const { runtimeId, sessionId, generation } = value as Record<string, unknown>;
	if (typeof runtimeId !== "string" || typeof sessionId !== "string" || !Number.isInteger(generation)) return undefined;
	return { runtimeId, sessionId, generation: generation as number };
}

export default function gitmootInbox(pi: Api) {
	const runtimeId = randomUUID();
	const loadedAt = Date.now();
	const endpoint = path.join(REGISTRY_DIR, `${runtimeId.replace(/-/g, "").slice(0, 12)}.sock`);
	const registrationPath = path.join(REGISTRY_DIR, `${runtimeId}.json`);

	let ctx: Context | undefined;
	let generation = 0;
	let tui: TuiHandle | undefined;
	let editor: unknown;
	let lastKeyAt = 0;
	let submissionAt = 0;
	let pasting = false;
	let manualCompactAt = 0;
	let autoCompactAt = 0;
	let retryAt = 0;
	let unsubscribeKeys: (() => void) | undefined;
	let server: net.Server | undefined;
	let registered = false;
	let lastError: string | undefined;

	const target = (): Target => ({ runtimeId, sessionId: ctx!.sessionManager.getSessionId(), generation });

	// The prompt editor is the focused component that reads back the same text
	// as ctx.ui.getEditorText() and keeps prompt history. Dialog inputs do not.
	function captureEditor(): void {
		if (!ctx || !tui || tui.hasOverlay?.()) return;
		const focused = tui.getFocused?.() as { getText?: () => string; addToHistory?: unknown } | null | undefined;
		if (typeof focused?.getText !== "function" || typeof focused.addToHistory !== "function") return;
		try {
			if (focused.getText() === ctx.ui.getEditorText()) editor = focused;
		} catch {
			// Leave the previous capture in place.
		}
	}

	function uiState() {
		const available = Boolean(tui) && typeof tui?.getFocused === "function" && typeof tui?.hasOverlay === "function";
		if (!available) return { tuiAvailable: false, overlay: false, editorFocused: false };
		return { tuiAvailable: true, overlay: Boolean(tui!.hasOverlay!()), editorFocused: editor !== undefined && tui!.getFocused!() === editor };
	}

	const active = (since: number, now: number) => since !== 0 && now - since < MAINTENANCE_CAP_MS;
	const submissionPending = (now: number) => submissionAt !== 0 && now - submissionAt < SUBMISSION_HOLD_MS;

	// Why an idle session cannot take a note right now, or undefined.
	function idleGate(now: number): Reason | undefined {
		if (submissionPending(now) || now - lastKeyAt < RECENT_KEY_MS) return "operator_active";
		const ui = uiState();
		// Without the TUI handle nothing proves the prompt is free: fail closed.
		if (!ui.tuiAvailable) return "dialog";
		if (now - Math.max(lastKeyAt, loadedAt) >= INACTIVITY_OVERRIDE_MS) return undefined;
		if (ui.overlay || !ui.editorFocused) return "dialog";
		if (ctx!.ui.getEditorText() !== "") return "draft";
		return undefined;
	}

	// The admission decision. Callers must act on it in the same synchronous turn.
	function admission(now: number): { reason: Reason } | { mode: "idle" | "working" } {
		if (active(manualCompactAt, now) || active(autoCompactAt, now) || active(retryAt, now)) return { reason: "busy" };
		if (!ctx!.isIdle()) return { mode: "working" };
		if (ctx!.hasPendingMessages()) return { reason: "busy" };
		const reason = idleGate(now);
		return reason ? { reason } : { mode: "idle" };
	}

	function deliver(id: unknown, request: Record<string, unknown>): Reply {
		const deferred = (reason: Reason) => ({ v: PROTOCOL_VERSION, id, status: "deferred", reason });
		const pinned = parseTarget(request.target);
		if (!pinned) return deferred("invalid");
		if (!ctx) return deferred("unavailable");
		const current = target();
		if (pinned.runtimeId !== current.runtimeId || pinned.sessionId !== current.sessionId || pinned.generation !== current.generation)
			return deferred("stale_session");
		if (typeof request.content !== "string" || request.content.trim() === "") return deferred("invalid");
		const decision = admission(Date.now());
		if ("reason" in decision) return deferred(decision.reason);
		const content = request.content.replace(CONTROL_CHARS, "");
		const ids = messageIds(content);
		try {
			pi.sendMessage(
				{ customType: NOTE_TYPE, content: formatNote(content, ids), display: true, details: { source: "gitmoot", messageIds: ids } },
				{ deliverAs: "aside" },
			);
		} catch (error) {
			lastError = `sendMessage: ${String(error)}`;
			return deferred("unavailable");
		}
		return { v: PROTOCOL_VERSION, id, status: "accepted", mode: decision.mode };
	}

	function probe(id: unknown): Reply {
		if (!ctx) return { v: PROTOCOL_VERSION, id, status: "deferred", reason: "unavailable" };
		const now = Date.now();
		const decision = admission(now);
		return {
			v: PROTOCOL_VERSION,
			id,
			status: "ok",
			target: target(),
			state: {
				addonVersion: ADDON_VERSION,
				pid: process.pid,
				idle: ctx.isIdle(),
				pendingMessages: ctx.hasPendingMessages(),
				...uiState(),
				draft: ctx.ui.getEditorText() !== "",
				msSinceKey: lastKeyAt === 0 ? null : now - lastKeyAt,
				submissionPending: submissionPending(now),
				compacting: active(manualCompactAt, now) || active(autoCompactAt, now),
				retrying: active(retryAt, now),
				admission: "reason" in decision ? { status: "deferred", reason: decision.reason } : { status: "accepted", mode: decision.mode },
				...(lastError ? { lastError } : {}),
			},
		};
	}

	function handle(line: string): Reply {
		let request: unknown;
		try {
			request = JSON.parse(line);
		} catch {
			return { v: PROTOCOL_VERSION, id: null, status: "deferred", reason: "invalid" };
		}
		if (!request || typeof request !== "object" || Array.isArray(request))
			return { v: PROTOCOL_VERSION, id: null, status: "deferred", reason: "invalid" };
		const fields = request as Record<string, unknown>;
		const id = typeof fields.id === "string" || typeof fields.id === "number" ? fields.id : null;
		if (fields.v !== PROTOCOL_VERSION) return { v: PROTOCOL_VERSION, id, status: "deferred", reason: "invalid" };
		if (fields.method === "probe") return probe(id);
		if (fields.method === "deliver") return deliver(id, fields);
		return { v: PROTOCOL_VERSION, id, status: "deferred", reason: "invalid" };
	}

	function serve(socket: net.Socket): void {
		let buffer = "";
		let answered = false;
		const answer = (reply: Reply) => {
			answered = true;
			socket.end(`${JSON.stringify(reply)}\n`);
		};
		socket.setEncoding("utf8");
		socket.setTimeout(REQUEST_TIMEOUT_MS, () => socket.destroy());
		socket.on("error", () => {});
		socket.on("data", (chunk: string) => {
			if (answered) return;
			buffer += chunk;
			const newline = buffer.indexOf("\n");
			if (newline >= 0) answer(handle(buffer.slice(0, newline)));
			else if (buffer.length > MAX_REQUEST_BYTES) answer({ v: PROTOCOL_VERSION, id: null, status: "deferred", reason: "invalid" });
		});
		socket.on("end", () => {
			if (!answered && buffer.trim() !== "") answer(handle(buffer));
		});
	}

	function ensureRegistryDir(): void {
		fs.mkdirSync(REGISTRY_DIR, { recursive: true, mode: 0o700 });
		const info = fs.lstatSync(REGISTRY_DIR);
		if (!info.isDirectory()) throw new Error(`${REGISTRY_DIR} is not a directory`);
		if (typeof process.getuid === "function" && info.uid !== process.getuid()) throw new Error(`${REGISTRY_DIR} is owned by another user`);
		if ((info.mode & 0o777) !== 0o700) fs.chmodSync(REGISTRY_DIR, 0o700);
	}

	function writeRegistration(): void {
		if (!ctx || !server?.listening) return;
		const record = {
			version: 1,
			runtimeId,
			sessionId: ctx.sessionManager.getSessionId(),
			generation,
			pid: process.pid,
			endpoint,
			herdrPaneId: process.env.HERDR_PANE_ID || null,
			updatedAt: new Date().toISOString(),
		};
		const temp = path.join(REGISTRY_DIR, `.${runtimeId}.json.tmp`);
		try {
			fs.writeFileSync(temp, `${JSON.stringify(record)}\n`, { mode: 0o600 });
			fs.chmodSync(temp, 0o600);
			fs.renameSync(temp, registrationPath);
			registered = true;
		} catch (error) {
			lastError = `registration: ${String(error)}`;
		}
	}

	function startServer(): void {
		if (server) return;
		try {
			if (!path.isAbsolute(REGISTRY_DIR)) throw new Error("registry dir is not absolute");
			if (Buffer.byteLength(endpoint) >= MAX_SOCKET_PATH_BYTES) throw new Error(`socket path is too long: ${endpoint}`);
			ensureRegistryDir();
			// A socket file left at our own random name can only be a leftover.
			if (fs.lstatSync(endpoint, { throwIfNoEntry: false })?.isSocket()) fs.unlinkSync(endpoint);
		} catch (error) {
			lastError = `registry: ${String(error)}`;
			return;
		}
		const created = net.createServer(serve);
		created.on("error", error => {
			lastError = `socket: ${String(error)}`;
		});
		created.listen(endpoint, () => {
			try {
				fs.chmodSync(endpoint, 0o600);
			} catch (error) {
				lastError = `socket: ${String(error)}`;
				created.close();
				return;
			}
			writeRegistration();
		});
		server = created;
		process.once("exit", cleanup);
	}

	function cleanup(): void {
		unsubscribeKeys?.();
		unsubscribeKeys = undefined;
		if (registered) fs.rmSync(registrationPath, { force: true });
		registered = false;
		if (server) {
			server.close();
			fs.rmSync(endpoint, { force: true });
			server = undefined;
		}
	}

	function onTerminalInput(data: string): undefined {
		if (TERMINAL_REPORTS.test(data)) return undefined;
		const now = Date.now();
		lastKeyAt = now;
		// Newlines inside a bracketed paste are text, not a submission.
		let typed = "";
		let rest = data;
		while (rest !== "") {
			if (pasting) {
				const end = rest.indexOf(PASTE_END);
				if (end < 0) break;
				pasting = false;
				rest = rest.slice(end + PASTE_END.length);
			} else {
				const start = rest.indexOf(PASTE_START);
				if (start < 0) {
					typed += rest;
					break;
				}
				typed += rest.slice(0, start);
				pasting = true;
				rest = rest.slice(start + PASTE_START.length);
			}
		}
		if (ENTER.test(typed)) submissionAt = now;
		return undefined;
	}

	function bind(next: Context): boolean {
		// Only the interactive top-level session owns the prompt. Subagents and
		// print mode have no operator to protect and are not delivery targets.
		if (!next.hasUI || next.agent?.kind === "sub") return false;
		ctx = next;
		manualCompactAt = autoCompactAt = retryAt = 0;
		submissionAt = 0;
		unsubscribeKeys?.();
		unsubscribeKeys = next.ui.onTerminalInput?.(onTerminalInput);
		// The documented widget factory receives the TUI; its public focus and
		// overlay methods tell whether the prompt editor is in front.
		next.ui.setWidget?.(WIDGET_KEY, handle => {
			tui = handle;
			captureEditor();
			return { render: () => [], invalidate: () => {} };
		});
		captureEditor();
		return true;
	}

	pi.on("session_start", (_event, next) => {
		if (!bind(next)) return;
		startServer();
		writeRegistration();
	});
	const switched = (_event: unknown, next: Context) => {
		// generation counts every switch (/new, resume, fork, branch) so the daemon
		// can tell a replaced conversation even if a session id were reused.
		if (!ctx || !bind(next)) return;
		generation++;
		writeRegistration();
	};
	pi.on("session_switch", switched);
	pi.on("session_branch", switched);
	pi.on("input", () => {
		submissionAt = Date.now();
		captureEditor();
	});
	pi.on("agent_start", () => {
		submissionAt = 0;
		manualCompactAt = 0;
		captureEditor();
	});
	pi.on("agent_end", () => {
		submissionAt = 0;
	});
	pi.on("session_before_compact", () => {
		manualCompactAt = Date.now();
	});
	pi.on("session_compact", () => {
		manualCompactAt = 0;
		autoCompactAt = 0;
	});
	pi.on("auto_compaction_start", () => {
		autoCompactAt = Date.now();
	});
	pi.on("auto_compaction_end", () => {
		autoCompactAt = 0;
	});
	pi.on("auto_retry_start", () => {
		retryAt = Date.now();
	});
	pi.on("auto_retry_end", () => {
		retryAt = 0;
	});
	pi.on("session_shutdown", cleanup);
}
