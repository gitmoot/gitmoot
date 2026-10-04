// Local stand-in model for `gitmoot plugin doctor omp --live`. It answers every
// request at once, never calls a network model, and records the context it was
// given so the check can confirm a delivered note reached the model.
import fs from "node:fs";

const logPath = process.env.GITMOOT_OMP_LIVECHECK_LOG;

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

export default function livecheckModel(pi: { registerProvider(name: string, config: unknown): void }) {
	const streamSimple = (model: MockModel, context: { messages?: unknown[] }) => {
		if (logPath) fs.appendFileSync(logPath, `${JSON.stringify({ messages: context.messages ?? [] })}\n`);
		const message = {
			role: "assistant",
			content: [{ type: "text", text: "gitmoot live check: note received." }],
			api: "gitmoot-livecheck",
			provider: model.provider,
			model: model.id,
			stopReason: "stop",
			timestamp: Date.now(),
			usage: ZERO_USAGE,
		};
		return {
			result: () => Promise.resolve(message),
			async *[Symbol.asyncIterator]() {
				yield { type: "start", partial: message };
				yield { type: "done", reason: "stop", message };
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
}
