// Local stand-in model for `gitmoot plugin doctor omp --live`. It answers every
// request at once, never calls a network model, and records the context it was
// given so the check can confirm a delivered note reached the model.
//
// It can also leave the session idle with a queued message that OMP does not
// drain by itself: when the check creates the control file it starts a turn,
// holds the model until that turn is aborted, queues a follow-up meanwhile and
// interrupts the turn. A user interrupt suppresses OMP's idle follow-up drain,
// so the follow-up stays queued until the next turn.
import fs from "node:fs";
import { setTimeout as sleep } from "node:timers/promises";

const logPath = process.env.GITMOOT_OMP_LIVECHECK_LOG;
const controlPath = process.env.GITMOOT_OMP_LIVECHECK_CONTROL;

const ZERO_USAGE = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	totalTokens: 0,
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

interface MockModel {
	provider: string;
	id: string;
}
interface Ctx {
	hasUI: boolean;
	isIdle(): boolean;
	hasPendingMessages(): boolean;
	abort(): void;
}
interface Api {
	registerProvider(name: string, config: unknown): void;
	on(event: string, handler: (event: unknown, ctx: Ctx) => unknown): void;
	sendMessage(
		message: { customType: string; content: string; display: boolean },
		options: { deliverAs?: "followUp"; triggerTurn?: boolean },
	): void;
}

export default function livecheckModel(pi: Api) {
	let hold = false;
	let held = false;

	const streamSimple = (model: MockModel, context: { messages?: unknown[] }, options?: { signal?: AbortSignal }) => {
		if (logPath) fs.appendFileSync(logPath, `${JSON.stringify({ messages: context.messages ?? [] })}\n`);
		const message = (stopReason: "stop" | "aborted") => ({
			role: "assistant",
			content: stopReason === "stop" ? [{ type: "text", text: "gitmoot live check: note received." }] : [],
			api: "gitmoot-livecheck",
			provider: model.provider,
			model: model.id,
			stopReason,
			timestamp: Date.now(),
			usage: ZERO_USAGE,
		});
		if (!hold) {
			const done = message("stop");
			return {
				result: () => Promise.resolve(done),
				async *[Symbol.asyncIterator]() {
					yield { type: "start", partial: done };
					yield { type: "done", reason: "stop", message: done };
				},
			};
		}
		// Answer nothing until the turn is aborted.
		hold = false;
		held = true;
		const signal = options?.signal;
		const { promise: aborted, resolve } = Promise.withResolvers<void>();
		if (!signal || signal.aborted) resolve();
		else signal.addEventListener("abort", () => resolve(), { once: true });
		const final = aborted.then(() => message("aborted"));
		return {
			result: () => final,
			async *[Symbol.asyncIterator]() {
				yield { type: "start", partial: message("aborted") };
				yield { type: "error", reason: "aborted", error: await final };
			},
		};
	};
	pi.registerProvider("gitmoot-livecheck", {
		baseUrl: "mock://gitmoot-livecheck",
		api: "gitmoot-livecheck",
		apiKey: "local-only",
		streamSimple,
		models: [
			{
				id: "mock",
				name: "Gitmoot live check (local)",
				input: ["text"],
				reasoning: false,
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
				contextWindow: 200000,
				maxTokens: 2048,
			},
		],
	});

	if (!controlPath) return;
	let ctx: Ctx | undefined;
	let busy = false;
	const report = (state: Record<string, unknown>) => fs.writeFileSync(`${controlPath}.done`, `${JSON.stringify(state)}\n`);
	const waitUntil = async (ready: () => boolean) => {
		for (let i = 0; i < 200 && !ready(); i++) await sleep(25);
		return ready();
	};
	async function strandQueue(content: string): Promise<void> {
		hold = true;
		held = false;
		pi.sendMessage({ customType: "gitmoot-livecheck", content: "gitmoot live check: hold this turn.", display: true }, { triggerTurn: true });
		if (!(await waitUntil(() => held && !ctx!.isIdle()))) return report({ error: "the held turn never reached the model" });
		pi.sendMessage({ customType: "gitmoot-livecheck", content, display: true }, { deliverAs: "followUp" });
		if (!(await waitUntil(() => ctx!.hasPendingMessages()))) return report({ error: "the follow-up was never queued" });
		ctx!.abort();
		if (!(await waitUntil(() => ctx!.isIdle()))) return report({ error: "the aborted turn never settled" });
		// Let OMP run any drain it would schedule for itself after the abort.
		await sleep(1000);
		report({ idle: ctx!.isIdle(), pendingMessages: ctx!.hasPendingMessages() });
	}
	pi.on("session_start", (_event, next) => {
		if (!next.hasUI) return;
		ctx = next;
		setInterval(() => {
			if (busy || !ctx || !fs.existsSync(controlPath)) return;
			busy = true;
			const content = fs.readFileSync(controlPath, "utf8");
			fs.rmSync(controlPath, { force: true });
			strandQueue(content)
				.catch(error => report({ error: String(error) }))
				.finally(() => {
					busy = false;
				});
		}, 50).unref();
	});
}
