package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// The fixtures below are synthetic-from-source (omp v17.2.4): they reproduce the
// shapes read out of oh-my-pi's print-mode writer and event union, not bytes
// captured from a live run (capturing those needs a provider credential, which is
// deliberately out of scope here). The loud-drift guard — a stream with no
// agent_end fails — is what protects the parser until a real capture lands.

const ompFixtureSessionID = "01920000-0000-7000-8000-000000000001"

// ompHeaderLine is omp's first stdout line under --mode=json: the raw
// SessionHeader, and the only channel that reports a finished print run's id.
func ompHeaderLine(id string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":%s,"timestamp":"2026-08-02T09:00:00.000Z","cwd":"/repo"}`,
		ompJSONString(id)) + "\n"
}

// ompAssistantEnd builds an assistant message_end event carrying text and the
// per-message usage block (note the input/output spelling omp uses there). Its
// stopReason is "stop": the one a run that FINISHED reports.
func ompAssistantEnd(text string, input, output int) string {
	return ompAssistantEndStopping(text, "stop", input, output)
}

// ompAssistantEndStopping is ompAssistantEnd with the stopReason under test. omp's
// union is closed — "stop" | "length" | "toolUse" | "error" | "aborted"
// (packages/ai/src/types.ts:792) — and "length" is the provider's output cap
// cutting a message mid-sentence, which is an ANSWER ONLY IF you ignore that it
// was cut.
func ompAssistantEndStopping(text string, stopReason string, input, output int) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%s}],`+
		`"usage":{"input":%d,"output":%d,"cacheRead":0,"cacheWrite":0,"totalTokens":%d},"stopReason":%s,`+
		`"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-4"}}`,
		ompJSONString(text), input, output, input+output, ompJSONString(stopReason)) + "\n"
}

// ompTextThenToolCallAssistantEnd is the turn that SPOKE and then called a tool:
// the shape a --max-time trip freezes in place. omp clears hasMoreToolCalls when
// the deadline has passed (agent-loop.ts:1283-1286) and settles the run terminally,
// so this message can be the LAST assistant message on a perfectly-formed stream
// while its sentence is a work note ("Let me read the file first.") rather than the
// job's answer.
func ompTextThenToolCallAssistantEnd(text string, stopReason string, input, output int) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%s},`+
		`{"type":"toolCall","toolCallId":"c1","toolName":"read","args":{"path":"/repo/a.go"}}],`+
		`"usage":{"input":%d,"output":%d,"totalTokens":%d},"stopReason":%s}}`,
		ompJSONString(text), input, output, input+output, ompJSONString(stopReason)) + "\n"
}

// ompAbortedToolResultEnd is the SYNTHETIC tool result omp pairs with every tool
// call a non-runnable turn left behind (agent-loop.ts:1349-1366): skipReason
// "aborted" with "Deadline exceeded" when the deadline tripped, "length" when the
// provider's output cap truncated the tool call. Its role is toolResult, so this
// parser's role filter drops it — it is on the wire here because a truncated stream
// really carries it, and because it is the ONLY place the word "aborted" appears on
// that wire: the assistant messages' own stopReason never says so.
func ompAbortedToolResultEnd(skipReason string, errorText string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"toolResult","toolCallId":"c1","skipReason":%s,`+
		`"content":[{"type":"text","text":%s}],"isError":true}}`,
		ompJSONString(skipReason), ompJSONString(errorText)) + "\n"
}

// ompAssistantEndNoUsage is the same event from a provider that reports no usage.
func ompAssistantEndNoUsage(text string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%s}],"stopReason":"stop"}}`,
		ompJSONString(text)) + "\n"
}

// ompFailedAssistantEnd builds the landmine event: an assistant turn that FAILED
// while the process still exits 0 under --mode=json.
func ompFailedAssistantEnd(stopReason, message string, status int) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"input":9,"output":0,"totalTokens":9},`+
		`"stopReason":%s,"errorMessage":%s,"errorStatus":%d}}`,
		ompJSONString(stopReason), ompJSONString(message), status) + "\n"
}

// ompToolResultEnd is a message_end that is NOT an assistant message. message_end
// fires for tool results too, so the role filter has to reject this one — its
// usage numbers are deliberately absurd so a missing filter is unmissable.
func ompToolResultEnd() string {
	return `{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"tool output"}],` +
		`"usage":{"input":1000,"output":1000,"totalTokens":2000}}}` + "\n"
}

// ompAssistantEndMixedParts is an assistant message whose content carries the
// part types omp actually mixes into one message: a thinking part (whose prose
// lives in a `thinking` field, not `text`), a toolCall part, and the real text.
// Only the text part may reach Summary.
func ompAssistantEndMixedParts(text string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"the user wants a review; let me stall"},`+
		`{"type":"toolCall","toolCallId":"c1","toolName":"read","args":{"path":"/repo/a.go"},"text":"LEAKED TOOL CALL"},`+
		`{"type":"text","text":%s}],"usage":{"input":3,"output":2},"stopReason":"stop"}}`,
		ompJSONString(text)) + "\n"
}

// ompAgentEnd is the terminal line every complete run emits.
func ompAgentEnd() string {
	return `{"type":"agent_end","messages":[],"isTerminal":true}` + "\n"
}

// ompAgentEndUntagged is an agent_end with NO isTerminal field: the raw AgentEvent
// shape (packages/agent/src/types.ts), since isTerminal is added by AgentSession
// when it re-emits the event. An absent field must read as terminal, or an omp
// build that stops tagging turns every run into a truncation error.
func ompAgentEndUntagged() string {
	return `{"type":"agent_end","messages":[]}` + "\n"
}

// ompAgentEndContinuation is a NON-terminal agent_end: agent-session.ts emits
// `{...event, isTerminal: !options?.willContinue}`, and eleven call sites pass
// willContinue:true (retries, model fallbacks, unexpected-stop recovery,
// auto-compaction).
//
// omp CONSTRUCTS one per continuation but normally does not WRITE it: while a
// prompt is in flight the wire-level agent_end is parked in a single-slot
// #pendingAgentEndEmit (agent-session.ts:1966-1969) and each later one supersedes
// the previous, so print mode's one session.prompt normally yields ONE agent_end —
// the final settle. This fixture is therefore the shape the adapter must survive,
// not the shape it should expect: a stream whose LAST agent_end looks like this was
// truncated between continuations.
func ompAgentEndContinuation() string {
	return `{"type":"agent_end","messages":[],"isTerminal":false}` + "\n"
}

// ompAgentEndWithTranscript is the realistic terminal agent_end: it re-serializes
// every message the run produced (`messages: AgentMessage[]`), written by
// print-mode.ts as ONE line with only providerPayload stripped. It is therefore
// structurally the LARGEST line in the stream and grows with every tool result —
// a few dozen 50 KB tool results put it past 1 MiB. padBytes sizes that transcript.
func ompAgentEndWithTranscript(padBytes int) string {
	return fmt.Sprintf(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"toolResult","content":[{"type":"text","text":%s}]}]}`,
		ompJSONString(strings.Repeat("x", padBytes))) + "\n"
}

// ompAgentEndTelemetryContinuation is a NON-terminal agent_end carrying a
// telemetry rollup: one agentLoop invocation's own segment. The rollup handle is
// "constructed once per `agentLoop` invocation" (telemetry.ts:400-412), so a retry
// or continuation starts a fresh collector and each rollup covers only its segment.
//
// Like ompAgentEndContinuation, this event is superseded on omp's normal wire
// (agent-session.ts:1966-1969) — which is exactly why the parser cannot infer
// coverage from counting agent_end events, and compares the rollup against the
// per-message sum instead.
func ompAgentEndTelemetryContinuation(input, output int) string {
	return fmt.Sprintf(`{"type":"agent_end","messages":[],"isTerminal":false,"telemetry":{"usage":{"inputTokens":%d,"outputTokens":%d,`+
		`"cachedInputTokens":0,"cacheWriteTokens":0,"reasoningOutputTokens":0,"totalTokens":%d},"stepCount":2}}`,
		input, output, input+output) + "\n"
}

// ompAutoRetryStart is the event omp emits AFTER the failed message_end, once it
// has decided the failure is retryable (5xx / overload / 429 usage limit / stream
// stall are all retryable, retry.enabled defaults true with 10 attempts).
func ompAutoRetryStart(attempt int, errorMessage string) string {
	return fmt.Sprintf(`{"type":"auto_retry_start","attempt":%d,"maxAttempts":10,"delayMs":2000,"errorMessage":%s}`,
		attempt, ompJSONString(errorMessage)) + "\n"
}

// ompAutoRetryEnd is omp's OWN verdict on a retry saga, and the event that CLOSES
// it on either verdict. omp emits one from every dead end and from the successful
// settle (turn-recovery.ts:224-249 for success:true; :257-261, :527-541, :1462-1470,
// :1490-1495 for success:false), because a saga left open latches every subscriber
// tracking retry-outstanding state (turn-recovery.ts:1481-1487).
func ompAutoRetryEnd(success bool, attempt int) string {
	return fmt.Sprintf(`{"type":"auto_retry_end","success":%t,"attempt":%d}`, success, attempt) + "\n"
}

// ompAutoRetryEndFinal is the giving-up verdict WITH the finalError omp attaches to
// it (agent-session-events.ts:42-47). For a saga whose every attempt ended in a
// content-less assistant stop, this string is the ONLY failure text on the wire:
// every message_end on that stream carries a non-error stopReason.
func ompAutoRetryEndFinal(attempt int, finalError string) string {
	return fmt.Sprintf(`{"type":"auto_retry_end","success":false,"attempt":%d,"finalError":%s}`,
		attempt, ompJSONString(finalError)) + "\n"
}

// ompEmptyAssistantEnd is a CONTENT-LESS assistant message_end — omp's own
// #isEmptyAssistantStop failure shape (turn-recovery.ts:554-576): a "stop" with no
// text and no toolCall, or a "toolUse" with no toolCall. Its stopReason is one this
// adapter's error check calls fine, so it is the failure that arrives disguised as a
// healthy turn. It is billed like any other attempt, hence the usage block.
func ompEmptyAssistantEnd(stopReason string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[],`+
		`"usage":{"input":7,"output":0,"totalTokens":7},"stopReason":%s}}`,
		ompJSONString(stopReason)) + "\n"
}

// ompWhitespaceAssistantEnd is the same failure shape carrying a whitespace-only
// text part: omp tests text content with hasNonWhitespace (/\S/), so this is an
// empty stop for it too.
func ompWhitespaceAssistantEnd(stopReason string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"  \n\t "}],`+
		`"usage":{"input":7,"output":1,"totalTokens":8},"stopReason":%s}}`,
		ompJSONString(stopReason)) + "\n"
}

// ompToolCallAssistantEnd is an assistant turn whose only content is a tool call:
// REAL content by omp's classifier (a toolCall makes both a "stop" and a "toolUse"
// non-empty), and therefore real recovery here.
func ompToolCallAssistantEnd() string {
	return ompToolCallAssistantEndStopping("toolUse")
}

// ompToolCallAssistantEndStopping is the same turn with the stopReason the provider
// reported. Both values are runnable for omp — `runnableStop = stopReason ===
// "toolUse" || stopReason === "stop"` (agent-loop.ts:1281) — so a provider that
// reports "stop" alongside tool calls produces a tool-calling turn whose stopReason
// is byte-identical to a FINISHED run's. When the deadline stops the loop there,
// nothing but the missing text distinguishes the truncation.
func ompToolCallAssistantEndStopping(stopReason string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","toolCallId":"c1",`+
		`"toolName":"read","args":{"path":"/repo/a.go"}}],"usage":{"input":8,"output":3,"totalTokens":11},"stopReason":%s}}`,
		ompJSONString(stopReason)) + "\n"
}

// ompAgentEndWithTelemetry is the agent_end omp emits when an OTEL telemetry
// config was supplied: it carries a run-level rollup that uses the OTHER usage
// spelling (inputTokens/outputTokens).
func ompAgentEndWithTelemetry(input, output int) string {
	return fmt.Sprintf(`{"type":"agent_end","messages":[],"telemetry":{"usage":{"inputTokens":%d,"outputTokens":%d,`+
		`"cachedInputTokens":0,"cacheWriteTokens":0,"reasoningOutputTokens":0,"totalTokens":%d},"stepCount":2}}`,
		input, output, input+output) + "\n"
}

// ompUndecodableMessageEnd is the row that made this whole rule necessary: VALID
// JSON, type "message_end" — a type this parser reads — whose per-message usage
// block reports `input` as a STRING. omp's own union types it as a number, so this
// is exactly the provider/envelope drift that shows up first in the field, and
// encoding/json fails the line as a whole. The message it hides is a FAILED final
// turn (stopReason error, a 529), which is why skipping the line silently turns the
// run into a success carrying whatever the PREVIOUS turn happened to say.
func ompUndecodableMessageEnd() string {
	return `{"type":"message_end","message":{"role":"assistant","content":[],` +
		`"usage":{"input":"7","output":0,"totalTokens":7},"stopReason":"error",` +
		`"errorMessage":"overloaded_error","errorStatus":529}}` + "\n"
}

// ompStringContentMessageEnd is the row rule 6 must NOT fail on: a message_end for a
// NON-assistant message whose `content` is a bare STRING. omp's unions permit it —
// UserMessage and DeveloperMessage type content as `string | (TextContent |
// ImageContent)[]` (packages/ai/src/types.ts:803-805, :817-819) and
// CustomMessageContent is the same union (coding-agent/src/session/messages.ts:314) —
// and omp really writes these on the print-mode wire: emitInputMessages pushes
// {message_start, message_end} for EVERY input/aside/steer/custom message of a turn
// (agent-loop.ts:938-943) and json mode prints every event verbatim
// (print-mode.ts:173-176). Go's []ompContentPart cannot decode a string, so each of
// these lines is valid JSON of a LOAD-BEARING type whose payload FAILS to decode,
// while being a row rule 2's role filter drops unread. Failing them would turn
// healthy runs into envelope-drift errors.
func ompStringContentMessageEnd(role, customType, content string) string {
	custom := ""
	if customType != "" {
		custom = fmt.Sprintf(`"customType":%s,"display":false,"attribution":"agent",`, ompJSONString(customType))
	}
	return fmt.Sprintf(`{"type":"message_end","message":{"role":%s,%s"content":%s,"timestamp":1785737730821}}`,
		ompJSONString(role), custom, ompJSONString(content)) + "\n"
}

// ompRolelessUndecodableMessageEnd is an undecodable message_end whose message object
// carries NO role at all: the discriminator that decides whether this parser would
// have read the row is itself missing, on a row it could not read.
func ompRolelessUndecodableMessageEnd() string {
	return `{"type":"message_end","message":{"content":"the content drifted to a string",` +
		`"usage":{"input":"7","output":0},"stopReason":"error","errorMessage":"overloaded_error"}}` + "\n"
}

// ompUnreadableRoleMessageEnd is the harder shape: `message` is not an object at all,
// so no probe can recover a role from it.
func ompUnreadableRoleMessageEnd() string {
	return `{"type":"message_end","message":"the whole payload drifted to a string"}` + "\n"
}

// ompDecodableFailedMessageEndWithoutRole is the bypass the g7 delta review probed at
// cb34f666, and it is NOT an undecodable row: content is a real part array, usage is
// numeric, stopReason is "error" — the whole payload decodes cleanly — but the role is
// null or missing, so Go zero-values ompStreamMessage.Role to "". roleField is the
// literal role member to splice in (`"role":null,`, or "" for a row that omits it).
//
// A row like this never reaches the undecodable classifier at all, and rule 2's
// `Role != "assistant"` filter reads the zero value as "some other role": a FAILED
// assistant turn shaped this way is skipped, every other envelope rule still passes,
// and the run reports SUCCESS carrying whatever an EARLIER turn happened to say.
func ompDecodableFailedMessageEndWithoutRole(roleField string) string {
	return fmt.Sprintf(`{"type":"message_end","message":{%s"content":[],"usage":{"input":9,"output":0,"totalTokens":9},`+
		`"stopReason":"error","errorMessage":"overloaded_error","errorStatus":529}}`, roleField) + "\n"
}

// ompDecodableMessageEndWithoutMessage is the third shape of that same bypass: a
// message_end whose `message` member is gone entirely. It decodes (unknown fields are
// ignored and a missing pointer is nil), so it too skips the undecodable classifier
// while carrying no discriminator at all.
func ompDecodableMessageEndWithoutMessage() string {
	return `{"type":"message_end","timestamp":1785737730821}` + "\n"
}

// ompMalformedAssistantMessageEndLine is a SYNTACTICALLY malformed message_end: the
// line was cut mid-write (an interleaved writer, a partial flush, a crash between
// chunks) so it is not valid JSON at all — yet the surviving prefix still IDENTIFIES
// the row, carrying a `"type"` of message_end and a `"role"` of assistant. When it is
// the FINAL assistant message of a run whose terminal agent_end still arrives, the
// missing-agent_end rules never fire and skipping it hands the previous turn's
// sentence back as the run's answer.
func ompMalformedAssistantMessageEndLine() string {
	return `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"the answer wa` + "\n"
}

// ompUndecodableRows is one valid-JSON, undecodable line per LOAD-BEARING event
// type, each a plausible drift of a field this parser really reads: a numeric
// session id, a stringified per-message usage count, a structured finalError, a
// stringified retry verdict, a stringified isTerminal. Keyed by event type so the
// table test can enumerate ompLoadBearingEventTypes and demand one per member.
var ompUndecodableRows = map[string]string{
	"session":          `{"type":"session","version":3,"id":42,"cwd":"/repo"}` + "\n",
	"message_end":      ompUndecodableMessageEnd(),
	"auto_retry_start": `{"type":"auto_retry_start","attempt":2,"maxAttempts":10,"finalError":{"code":529}}` + "\n",
	"auto_retry_end":   `{"type":"auto_retry_end","success":"false","attempt":3}` + "\n",
	"agent_end":        `{"type":"agent_end","messages":[],"isTerminal":"true"}` + "\n",
}

func ompJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// ompStreamOK is the minimal happy-path envelope: header, one assistant
// message_end with text and usage, and the terminal agent_end. Shared with the
// runtime tables in adapter_test.go.
var ompStreamOK = ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd("done", 10, 5) + ompAgentEnd()

func ompTestAgent() Agent {
	return Agent{
		Name:           "reviewer",
		Role:           "reviewer",
		Runtime:        OmpRuntime,
		RuntimeRef:     ompFixtureSessionID,
		RepoScope:      "gitmoot/gitmoot",
		AutonomyPolicy: AutonomyPolicyReadOnly,
	}
}

// ompResumeFlags are the flags v1 must NEVER emit. --resume/--continue/--fork
// relocate the process to the RESUMED session's cwd (switchToResumedProject
// overwrites the parsed --cwd), so a job would edit the previous worktree and
// leave its own clean; --session-dir would accrete per-worktree session state for
// a runtime that never resumes; --yolo/--auto-approve are the aliases the explicit
// --approval-mode replaces; and --profile is deferred until per-seat isolation is
// designed.
//
// --plan-yolo is deliberately ABSENT from this list and is NOT unguarded: a
// blanket ban would be weaker than what the adapter now owes. It is governed by
// assertOmpPlanArgv's IF-AND-ONLY-IF assertion instead, which subsumes the ban
// (no plan request => the flag must not appear in any spelling) and additionally
// catches the two failures a ban cannot see: a plan request whose flag went
// MISSING, and a plan run that lost --approval-mode=yolo.
var ompForbiddenFlags = []string{
	"--resume", "--continue", "--fork", "--session-dir",
	"--yolo", "--auto-approve", "--profile",
}

func assertNoForbiddenOmpFlags(t *testing.T, argv []string) {
	t.Helper()
	for _, arg := range argv {
		for _, forbidden := range ompForbiddenFlags {
			if arg == forbidden || strings.HasPrefix(arg, forbidden+"=") {
				t.Fatalf("argv carries forbidden flag %q: %v", forbidden, argv)
			}
		}
	}
	// Every omp argv, plan or not, carries the explicit yolo approval mode: plan
	// mode is workflow shape, the approval mode is write permission, and omp bricks
	// headlessly under always-ask.
	if got := ompCountToken(argv, "--approval-mode=yolo"); got != 1 {
		t.Fatalf("--approval-mode=yolo appears %d times, want exactly 1: %v", got, argv)
	}
}

// assertOmpPlanArgv is the plan-mode IF-AND-ONLY-IF guard: --plan-yolo appears
// exactly when the job asked for plan mode and NEVER otherwise, --plan-yolo-into
// appears exactly when a target is resolvable, it carries that target, it directly
// follows --plan-yolo (omp only accepts the pair), and neither flag ever shows up
// in an `=` spelling the adapter does not emit. --approval-mode=yolo is
// re-asserted on both sides so plan mode can never be mistaken for an approval
// setting.
//
// wantTarget is what the CALLER expects, stated independently rather than
// recomputed through ompPlanTarget: a test that asks production for the answer it
// is checking passes through any change to the resolution rule — including the
// silent-@smol behaviour this guard now exists to prevent.
func assertOmpPlanArgv(t *testing.T, argv []string, plan bool, wantTarget string) {
	t.Helper()
	wantTarget = strings.TrimSpace(wantTarget)
	wantPlan, wantInto := 0, 0
	if plan {
		wantPlan = 1
		if wantTarget != "" {
			wantInto = 1
		}
	}
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--plan-yolo") && arg != "--plan-yolo" && arg != "--plan-yolo-into" {
			t.Fatalf("argv carries an unrecognized plan flag spelling %q: %v", arg, argv)
		}
	}
	if got := ompCountToken(argv, "--plan-yolo"); got != wantPlan {
		t.Fatalf("--plan-yolo appears %d times, want %d (plan=%v): %v", got, wantPlan, plan, argv)
	}
	if got := ompCountToken(argv, "--plan-yolo-into"); got != wantInto {
		t.Fatalf("--plan-yolo-into appears %d times, want %d (plan=%v target=%q): %v", got, wantInto, plan, wantTarget, argv)
	}
	if wantInto == 1 {
		if got := ompFlagValue(argv, "--plan-yolo-into"); got != wantTarget {
			t.Fatalf("--plan-yolo-into = %q, want %q: %v", got, wantTarget, argv)
		}
		for i, arg := range argv {
			if arg == "--plan-yolo-into" && (i == 0 || argv[i-1] != "--plan-yolo") {
				t.Fatalf("--plan-yolo-into must directly follow --plan-yolo (omp rejects it alone): %v", argv)
			}
		}
	}
	if got := ompCountToken(argv, "--approval-mode=yolo"); got != 1 {
		t.Fatalf("--approval-mode=yolo appears %d times, want exactly 1 (plan=%v): %v", got, plan, argv)
	}
}

func ompFlagValue(argv []string, flag string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func ompCountToken(argv []string, token string) int {
	count := 0
	for _, arg := range argv {
		if arg == token {
			count++
		}
	}
	return count
}

// TestOmpDeliverFreshArgv pins the argument vector byte for byte. Kills: a dropped
// --mode=json (omp prints prose and every job fails extraction) and a prompt that
// lands before `--` (reparsed as flags or as an @attachment).
func TestOmpDeliverFreshArgv(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review this PR"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", "review this PR")
	})

	t.Run("model effort and workspace grants", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		agent := ompTestAgent()
		// The blank entry must be skipped, and writable paths must precede readable
		// ones (the kimi ordering).
		agent.WritablePaths = []string{"/work/out", "  ", "/work/cache"}
		agent.ReadablePaths = []string{"/inputs"}
		job := Job{Prompt: "implement it", Model: "anthropic/claude-opus-4", Effort: "high"}
		if _, err := adapter.Deliver(context.Background(), agent, job); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session",
			"--add-dir", "/work/out", "--add-dir", "/work/cache", "--add-dir", "/inputs",
			"--model", "anthropic/claude-opus-4", "--thinking", "high", "--", "implement it")
	})

	t.Run("unknown effort drops the flag", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "go", Effort: "ludicrous"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", "go")
	})

	// The FULL model/effort precedence chain, byte-exact: job override > agent
	// default > runtime registry default (EffectiveModel / effectiveEffort, #652).
	// Kills: reading job.Model / job.Effort directly, which silently drops a seat's
	// own --model and the registry's default_model — the run lands on omp's built-in
	// default with no signal anywhere.
	t.Run("model and effort precedence", func(t *testing.T) {
		cases := []struct {
			name        string
			agentModel  string
			agentEffort string
			job         Job
			wantModel   string
			wantEffort  string
		}{
			{
				name:       "agent defaults reach the argv with an empty job",
				agentModel: "anthropic/claude-opus-4", agentEffort: "high",
				job:       Job{Prompt: "work"},
				wantModel: "anthropic/claude-opus-4", wantEffort: "high",
			},
			{
				name:      "runtime registry defaults apply when neither agent nor job pins one",
				job:       Job{Prompt: "work", RuntimeDefaultModel: "openai/gpt-5.5", RuntimeDefaultEffort: "medium"},
				wantModel: "openai/gpt-5.5", wantEffort: "medium",
			},
			{
				name:       "the job overrides both the agent default and the registry default",
				agentModel: "anthropic/claude-opus-4", agentEffort: "high",
				job: Job{
					Prompt: "work",
					Model:  "openai/gpt-5.5-codex", Effort: "low",
					RuntimeDefaultModel: "zai/glm-5", RuntimeDefaultEffort: "max",
				},
				wantModel: "openai/gpt-5.5-codex", wantEffort: "low",
			},
			{
				name:       "the agent default beats the registry default",
				agentModel: "anthropic/claude-opus-4", agentEffort: "high",
				job:       Job{Prompt: "work", RuntimeDefaultModel: "zai/glm-5", RuntimeDefaultEffort: "max"},
				wantModel: "anthropic/claude-opus-4", wantEffort: "high",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
				adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
				agent := ompTestAgent()
				agent.Model = tc.agentModel
				agent.Effort = tc.agentEffort
				if _, err := adapter.Deliver(context.Background(), agent, tc.job); err != nil {
					t.Fatalf("Deliver: %v", err)
				}
				runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session",
					"--model", tc.wantModel, "--thinking", tc.wantEffort, "--", "work")
			})
		}
	})
}

// TestOmpDeliverNeverResumes proves the ref cannot reach the argv in any shape.
// Kills: any --resume/--continue/--fork, which would relocate the run to the
// resumed session's cwd and leave the job's own worktree clean — a green job with
// an empty diff.
func TestOmpDeliverNeverResumes(t *testing.T) {
	wantArgv := []string{"omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", "work"}
	tests := []struct {
		name     string
		ref      string
		delivers bool
	}{
		{name: "empty", ref: "", delivers: true},
		{name: "fresh", ref: "fresh:seat-reviewer", delivers: true},
		{name: "uuid", ref: ompFixtureSessionID, delivers: true},
		// "last" is another runtime's resume grammar: omp rejects it outright
		// rather than quietly treating it as a fresh run.
		{name: "last", ref: LastRef, delivers: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			agent := ompTestAgent()
			agent.RuntimeRef = tc.ref
			_, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "work"})
			if tc.delivers {
				if err != nil {
					t.Fatalf("Deliver: %v", err)
				}
				if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], wantArgv) {
					t.Fatalf("argv = %v, want %v (identical for every ref)", runner.calls, wantArgv)
				}
			} else {
				if err == nil {
					t.Fatal("Deliver accepted a resume-style ref")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("rejected ref still ran %d subprocess(es): %v", len(runner.calls), runner.calls)
				}
			}
			for _, call := range runner.calls {
				assertNoForbiddenOmpFlags(t, call)
				// No plan was requested, so no plan flag may exist in any spelling.
				assertOmpPlanArgv(t, call, false, "")
			}
		})
	}
}

// TestOmpDeliverIsSessionless kills a dropped --no-session (per-worktree session
// .jsonl accretion for a runtime that never resumes) and an added --session-dir.
func TestOmpDeliverIsSessionless(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	argv := runner.calls[0]
	if got := ompCountToken(argv, "--no-session"); got != 1 {
		t.Fatalf("--no-session appears %d times, want exactly 1: %v", got, argv)
	}
	assertNoForbiddenOmpFlags(t, argv)
	assertOmpPlanArgv(t, argv, false, "")
}

// TestOmpPlanArgv pins the plan-mode argv byte for byte on BOTH sides of the
// if-and-only-if: a job that asked for nothing gets no plan flag, a plan job gets
// --plan-yolo, and a pinned execution model gets --plan-yolo-into directly after
// it. --approval-mode=yolo survives every variant — plan mode is workflow shape,
// the approval mode is write permission, and mapping one onto the other would
// brick the runtime under always-ask. Result.PlanMode is asserted alongside so
// the durable evidence cannot drift from the argv it describes.
func TestOmpPlanArgv(t *testing.T) {
	base := []string{"omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session"}
	cases := []struct {
		name         string
		job          Job
		wantTail     []string
		wantPlanMode string
	}{
		{
			name:     "no plan request emits no plan flag",
			job:      Job{Prompt: "work"},
			wantTail: []string{"--", "work"},
		},
		{
			// A bare plan request with no model anywhere is the ONE case Gitmoot
			// cannot name a target for, so the flag stays off and omp applies its
			// own default. The evidence says so rather than claiming a pin.
			name:         "plan with no model resolvable leaves the target to the runtime",
			job:          Job{Prompt: "work", Plan: true},
			wantTail:     []string{"--plan-yolo", "--", "work"},
			wantPlanMode: "plan-into:<runtime-default>",
		},
		{
			// THE REGRESSION GUARD: the execution phase is a model switch, so a
			// job's pin must reach it. Before this, a bare --plan-yolo let omp
			// resolve @smol and the cheap default wrote the diff while the pinned
			// model only planned.
			name:         "the job model carries into the execution phase when plan_into is absent",
			job:          Job{Prompt: "work", Model: "openai/gpt-5.5", Plan: true},
			wantTail:     []string{"--model", "openai/gpt-5.5", "--plan-yolo", "--plan-yolo-into", "openai/gpt-5.5", "--", "work"},
			wantPlanMode: "plan-into:openai/gpt-5.5",
		},
		{
			name:         "plan_into pins the execution model immediately after --plan-yolo",
			job:          Job{Prompt: "work", Plan: true, PlanInto: "@smol"},
			wantTail:     []string{"--plan-yolo", "--plan-yolo-into", "@smol", "--", "work"},
			wantPlanMode: "plan-into:@smol",
		},
		{
			// An explicit plan_into always wins over the job's model: the caller
			// asking for a cheap executor is a choice, not an accident.
			name:         "an explicit plan_into overrides the job model",
			job:          Job{Prompt: "work", Model: "openai/gpt-5.5", Plan: true, PlanInto: "@smol"},
			wantTail:     []string{"--model", "openai/gpt-5.5", "--plan-yolo", "--plan-yolo-into", "@smol", "--", "work"},
			wantPlanMode: "plan-into:@smol",
		},
		{
			name:         "a blank plan_into falls through to the job model, not to the runtime default",
			job:          Job{Prompt: "work", Model: "openai/gpt-5.5", Plan: true, PlanInto: "   "},
			wantTail:     []string{"--model", "openai/gpt-5.5", "--plan-yolo", "--plan-yolo-into", "openai/gpt-5.5", "--", "work"},
			wantPlanMode: "plan-into:openai/gpt-5.5",
		},
		{
			// The documented order: model/thinking first, then the plan pair, then
			// the prompt. A plan flag that drifted after `--` would be read as
			// message text and the run would silently be a normal one.
			name:         "plan follows model and effort in the documented order",
			job:          Job{Prompt: "work", Model: "openai/gpt-5.5", Effort: "high", Plan: true, PlanInto: "anthropic/claude-haiku-4"},
			wantTail:     []string{"--model", "openai/gpt-5.5", "--thinking", "high", "--plan-yolo", "--plan-yolo-into", "anthropic/claude-haiku-4", "--", "work"},
			wantPlanMode: "plan-into:anthropic/claude-haiku-4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			result, err := adapter.Deliver(context.Background(), ompTestAgent(), tc.job)
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			want := append(append([]string(nil), base...), tc.wantTail...)
			runner.want(t, 0, want...)
			// The target the argv must carry is read back out of the evidence string
			// each case already declares, so argv and plan_mode are checked against
			// ONE stated expectation instead of two that could drift apart.
			wantTarget := strings.TrimPrefix(tc.wantPlanMode, "plan-into:")
			if wantTarget == tc.wantPlanMode || wantTarget == "<runtime-default>" {
				wantTarget = ""
			}
			assertOmpPlanArgv(t, runner.calls[0], tc.job.Plan, wantTarget)
			assertNoForbiddenOmpFlags(t, runner.calls[0])
			if result.PlanMode != tc.wantPlanMode {
				t.Fatalf("Result.PlanMode = %q, want %q", result.PlanMode, tc.wantPlanMode)
			}
		})
	}
}

// TestOmpPlanEvidenceSurvivesFailure closes surviving mutant M8: dropping
// `PlanMode: planMode` from BOTH failure returns left the whole suite green, even
// though three separate comments in this codebase insist "a plan run that died is
// still a plan run". The only test reading Result.PlanMode fed a healthy stream in
// every case, so the two error returns were never observed. A plan run's evidence
// matters MOST when it failed — that is when someone asks what was dispatched.
func TestOmpPlanEvidenceSurvivesFailure(t *testing.T) {
	job := Job{Prompt: "work", Model: "openai/gpt-5.5", Plan: true, PlanInto: "@smol"}
	const want = "plan-into:@smol"
	t.Run("non-zero exit still reports the plan shape", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}, errs: []error{errors.New("exit status 1")}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), job)
		if err == nil {
			t.Fatal("Deliver succeeded on a failing runner")
		}
		if result.PlanMode != want {
			t.Fatalf("Result.PlanMode on the non-zero-exit return = %q, want %q", result.PlanMode, want)
		}
	})
	t.Run("an unparseable stream still reports the plan shape", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: "not ndjson at all\n"}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), job)
		if err == nil {
			t.Fatal("Deliver succeeded on an unparseable stream")
		}
		if result.PlanMode != want {
			t.Fatalf("Result.PlanMode on the parse-error return = %q, want %q", result.PlanMode, want)
		}
	})
}

// TestOmpPlanPairPrecedesAttachment closes surviving mutant M18: moving the whole
// plan block after the attachment append left the suite green, because every
// byte-exact case used a short prompt and so never staged one. The documented argv
// shape puts the plan pair BEFORE the attachment, and omp reads a `@`-prefixed
// token as a file only when it was not already consumed as a flag value — so the
// ordering is load-bearing, not cosmetic.
func TestOmpPlanPairPrecedesAttachment(t *testing.T) {
	argv := ompArgs(Agent{}, "openai/gpt-5.5", "", "", true, "@smol", []string{"@/staged/prompt.md"}, "work")
	planAt, attachAt := -1, -1
	for i, a := range argv {
		if a == "--plan-yolo" && planAt < 0 {
			planAt = i
		}
		if a == "@/staged/prompt.md" {
			attachAt = i
		}
	}
	if planAt < 0 || attachAt < 0 {
		t.Fatalf("expected both a plan flag and a staged attachment: %v", argv)
	}
	if planAt > attachAt {
		t.Fatalf("plan pair must precede the staged attachment (omp only reads @tokens it did not consume as a flag value): %v", argv)
	}
}

// TestOmpPlanValidationPrecedesPreflight closes surviving mutant M12: moving the
// ompValidatePlan call after the PATH preflight left the suite green, because the
// test runner's LookPath always succeeds so the ordering was unobservable. Plan
// shape is a property of the REQUEST, so its diagnosis must not depend on whether
// omp happens to be installed — an operator with a malformed plan_into on a host
// without omp should be told about the plan_into.
func TestOmpPlanValidationPrecedesPreflight(t *testing.T) {
	adapter := OmpAdapter{Runner: &lookPathFailRunner{}, Dir: "/repo"}
	_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work", Plan: true, PlanInto: "--add-dir=/"})
	if err == nil {
		t.Fatal("Deliver accepted a malformed plan target")
	}
	if !strings.Contains(err.Error(), "plan target") {
		t.Fatalf("with omp absent AND a malformed plan target, the error must name the plan target, got: %v", err)
	}
}

// lookPathFailRunner reports omp as absent so the PATH preflight would fail if it
// ran first.
type lookPathFailRunner struct{ fakeRunner }

func (r *lookPathFailRunner) LookPath(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}

// TestOmpArgsPlanIffAcrossShapes runs the if-and-only-if guard over the whole
// primitive matrix the argv builder accepts. It is the "never otherwise" half:
// no combination of model, effort, workspace grants, max-time, or attachments may
// conjure a plan flag, and none may swallow one that was asked for.
func TestOmpArgsPlanIffAcrossShapes(t *testing.T) {
	agents := map[string]Agent{
		"bare":   {},
		"grants": {WritablePaths: []string{"/work/out"}, ReadablePaths: []string{"/inputs"}},
	}
	extras := map[string][]string{
		"no attachment": nil,
		"attachment":    {"@/staged/prompt.md"},
	}
	for agentName, agent := range agents {
		for extraName, attach := range extras {
			for _, model := range []string{"", "openai/gpt-5.5"} {
				for _, thinking := range []string{"", "high"} {
					for _, maxTime := range []string{"", "600"} {
						for _, plan := range []bool{false, true} {
							for _, into := range []string{"", "@smol"} {
								if !plan && into != "" {
									continue // rejected before dispatch; not representable
								}
								argv := append([]string{"omp"}, ompArgs(agent, model, thinking, maxTime, plan, into, attach, "work")...)
								// The rule, restated here rather than borrowed from
								// production: an explicit plan_into wins, else the
								// job's model carries into the execution phase, else
								// no flag and omp picks. Stating it independently is
								// what makes this a check and not an echo.
								wantTarget := into
								if wantTarget == "" {
									wantTarget = model
								}
								assertOmpPlanArgv(t, argv, plan, wantTarget)
								assertNoForbiddenOmpFlags(t, argv)
								if got := argv[len(argv)-2]; got != "--" {
									t.Fatalf("%s/%s: prompt is not the single token after `--`: %v", agentName, extraName, argv)
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestOmpDeliverRejectsPlanIntoWithoutPlan: upstream omp only accepts
// --plan-yolo-into alongside --plan-yolo, so the unpaired shape is refused HERE,
// before any subprocess, with the fix in the message. Silently dropping the
// target — or shipping the flag alone and letting omp exit non-zero with no
// envelope, which this adapter would diagnose as a truncated stream — are both
// worse than a named rejection.
func TestOmpDeliverRejectsPlanIntoWithoutPlan(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work", PlanInto: "@smol"})
	if err == nil {
		t.Fatal("Deliver accepted plan_into without plan")
	}
	if !strings.Contains(err.Error(), "--plan-yolo-into") || !strings.Contains(err.Error(), "--plan-yolo") {
		t.Fatalf("error must name both flags so the fix is obvious, got: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("rejected plan shape still ran %d subprocess(es): %v", len(runner.calls), runner.calls)
	}
}

func TestOmpDeliverRejectsMalformedPlanInto(t *testing.T) {
	for _, target := range []string{"x --add-dir /etc", "--model", "provider/model\nnext"} {
		t.Run(strings.ReplaceAll(target, "\n", "_newline_"), func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work", Plan: true, PlanInto: target})
			if err == nil {
				t.Fatalf("Deliver accepted malformed plan_into %q", target)
			}
			for _, want := range []string{"omp plan target", "one non-flag model selector", "plan_into"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if len(runner.calls) != 0 {
				t.Fatalf("rejected plan target still ran %d subprocess(es): %v", len(runner.calls), runner.calls)
			}
		})
	}
}

// TestOmpStartNeverPlans: Start registers a seat, it does not run a brief, so its
// argv must never carry a plan flag no matter what the agent looks like.
func TestOmpStartNeverPlans(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	agent := ompTestAgent()
	agent.RuntimeRef = ""
	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "introduce yourself"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertOmpPlanArgv(t, runner.calls[0], false, "")
	assertNoForbiddenOmpFlags(t, runner.calls[0])
}

// TestSupportsPlanMode pins the closed set. omp is the only runtime with the
// flag; every other runtime must be rejected at dispatch rather than run a
// plan-gated brief as an ordinary implementation.
func TestSupportsPlanMode(t *testing.T) {
	if !SupportsPlanMode(OmpRuntime) || !SupportsPlanMode("  "+OmpRuntime+"  ") {
		t.Fatal("omp must support plan mode")
	}
	for _, name := range []string{CodexRuntime, ClaudeRuntime, KimiRuntime, KimiCLIRuntime, ShellRuntime, "", "bogus"} {
		if SupportsPlanMode(name) {
			t.Fatalf("runtime %q must not claim plan mode", name)
		}
	}
}

// TestPlanModeDescriptor pins the durable evidence strings a reader sees instead
// of re-deriving plan mode from an argv.
func TestPlanModeDescriptor(t *testing.T) {
	cases := []struct {
		plan bool
		into string
		want string
	}{
		{plan: false, into: "", want: ""},
		{plan: false, into: "@smol", want: ""},
		// Plan mode ALWAYS executes on some model, so the evidence never says a
		// bare "plan": either Gitmoot resolved the target and names it, or it did
		// not and says the runtime chose. A bare "plan" would let a diff written
		// by the runtime's cheap default look identical to one written by the
		// pinned model — the exact ambiguity this string exists to remove.
		{plan: true, into: "", want: "plan-into:<runtime-default>"},
		{plan: true, into: "  ", want: "plan-into:<runtime-default>"},
		{plan: true, into: " @smol ", want: "plan-into:@smol"},
		{plan: true, into: "openai/gpt-5.5", want: "plan-into:openai/gpt-5.5"},
	}
	for _, tc := range cases {
		if got := PlanModeDescriptor(tc.plan, tc.into); got != tc.want {
			t.Fatalf("PlanModeDescriptor(%v, %q) = %q, want %q", tc.plan, tc.into, got, tc.want)
		}
	}
}

// TestOmpDeliverStopReasonErrorFailsOnExitZero is the omp landmine: under
// --mode=json a failed turn exits 0 (process.exit(1) is inside the text-mode
// branch). Kills: trusting the exit code, which would report a provider failure as
// a successful empty job.
func TestOmpDeliverStopReasonErrorFailsOnExitZero(t *testing.T) {
	for _, stopReason := range []string{"error", "aborted"} {
		t.Run(stopReason, func(t *testing.T) {
			stdout := ompHeaderLine(ompFixtureSessionID) +
				ompFailedAssistantEnd(stopReason, "upstream is overloaded", 529) +
				ompAgentEnd()
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
			if err == nil {
				t.Fatalf("Deliver succeeded on a %s turn that exited 0", stopReason)
			}
			for _, want := range []string{stopReason, "529", "upstream is overloaded"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q must carry %q", err.Error(), want)
				}
			}
			if result.Raw != stdout {
				t.Fatalf("Raw = %q, want the stdout evidence", result.Raw)
			}
			// A failed omp turn is BILLED. The dominant failure mode is exit 0 with
			// stopReason error, and omp retries it up to 10 times by default with every
			// attempt billed, so discarding the usage the parser already accumulated
			// books 0/0 for the runs that spend the most. The fixture's failed turn
			// carries usage 9/0.
			if result.InputTokens != 9 || result.OutputTokens != 0 {
				t.Fatalf("usage = %d/%d, want the failed turn's billed 9/0 (a failed turn is still billed)",
					result.InputTokens, result.OutputTokens)
			}
			diag := result.SessionDiag
			if diag == nil {
				t.Fatal("SessionDiag = nil, want process evidence for a run that happened")
			}
			if diag.ExitCode == nil || *diag.ExitCode != 0 {
				t.Fatalf("ExitCode = %v, want 0 (that is the whole point of this test)", diag.ExitCode)
			}
			if diag.SessionID != ompFixtureSessionID {
				t.Fatalf("SessionID = %q, want the header id %q", diag.SessionID, ompFixtureSessionID)
			}
		})
	}
}

// TestOmpDeliverEmptyAssistantTextIsError kills the worst silent failure: an empty
// review being read as "nothing to flag".
//
// Both shapes are pinned. The exactly-empty string is the easy one; the REALISTIC
// one is whitespace-only, which a `len(text) == 0` guard lets through — Deliver
// would then return Summary="" with Raw="  \n ", the engine would fall back to Raw
// (workflow/mailbox.go) and the job would die as envelope drift instead of saying
// the stream carried no assistant text.
func TestOmpDeliverEmptyAssistantTextIsError(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{name: "whitespace only", text: "  \n "},
		{name: "exactly empty", text: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd(tc.text, 4, 0) + ompAgentEnd()
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
			if err == nil {
				t.Fatalf("Deliver returned success for a %s assistant response: %+v", tc.name, result)
			}
			if !strings.Contains(err.Error(), "no assistant text") {
				t.Fatalf("error %q must say the stream carried no assistant text", err.Error())
			}
			if result.Raw != stdout {
				t.Fatalf("Raw = %q, want the stdout evidence", result.Raw)
			}
		})
	}
}

// TestOmpDeliverTruncatedRunNeverAnswersWithEarlierText is the TERMINAL half of the
// stale-text rule, and the one this adapter manufactures work for itself: a run that
// was CUT must never be answered with a sentence it wrote before the cut.
//
// The trigger is ompMaxTimeArg. Every daemon-dispatched job carries a context
// deadline, so every job gets --max-time floor(0.9 x remaining), and omp treats it
// as a HARD deadline INSIDE the tool loop: hasMoreToolCalls is cleared
// (agent-loop.ts:1283-1286), each pending tool call is paired with a synthetic
// `aborted` tool result (:1349-1366), and endAgentStream writes a TERMINAL agent_end
// (:1398-1400). The provider's output cap produces the same shape via stopReason
// "length" (:1352). So the envelope is perfect — one terminal agent_end, no error
// stopReason, no open saga — and the ONLY evidence of the cut is that the last
// assistant message never answered.
//
// Kills, one per shape:
//   - tracking the last NON-EMPTY assistant text instead of the FINAL message's
//     (the first case: its stopReason is the same "stop" a finished run reports, so
//     nothing else in the parser can fail it);
//   - dropping "toolUse"/"length" from ompStopReasonIsTruncation (the cases whose
//     final message DOES carry text — the terminal-text rule passes them).
func TestOmpDeliverTruncatedRunNeverAnswersWithEarlierText(t *testing.T) {
	const stale = "Let me read the file first."
	cases := []struct {
		name       string
		stdout     string
		wantInput  int
		wantOutput int
		why        string
	}{
		{
			// Only the terminal-TEXT rule can fail this stream: its final stopReason is
			// "stop", which a finished run also reports.
			name: "final turn is a tool call it never got to answer",
			stdout: ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd(stale, 17, 8) +
				ompToolResultEnd() +
				ompToolCallAssistantEndStopping("stop") +
				ompAbortedToolResultEnd("aborted", "Deadline exceeded") +
				ompAgentEnd(),
			wantInput: 25, wantOutput: 11,
			why: "the run stopped holding a tool call, so the earlier work note is all it ever said",
		},
		{
			// The reviewer's reproduction, verbatim in shape: two tool-calling turns, the
			// second carrying no text at all.
			name: "deadline trips mid tool loop (the --max-time shape)",
			stdout: ompHeaderLine(ompFixtureSessionID) +
				ompTextThenToolCallAssistantEnd(stale, "toolUse", 17, 8) +
				ompToolResultEnd() +
				ompToolCallAssistantEndStopping("toolUse") +
				ompAbortedToolResultEnd("aborted", "Deadline exceeded") +
				ompAgentEnd(),
			wantInput: 25, wantOutput: 11,
			why: "omp's own deadline stopped the loop and settled the run terminally",
		},
		{
			// Final message CARRIES text: the terminal-text rule passes it, so only the
			// stopReason rule stands between a half-sentence and the job's Summary.
			name: "deadline trips on a turn that spoke before calling its tool",
			stdout: ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd(stale, 17, 8) +
				ompToolResultEnd() +
				ompTextThenToolCallAssistantEnd("Now let me check the tests.", "toolUse", 6, 4) +
				ompAbortedToolResultEnd("aborted", "Deadline exceeded") +
				ompAgentEnd(),
			wantInput: 23, wantOutput: 12,
			why: "a work note plus a tool call is not a verdict, however recent it is",
		},
		{
			name: "provider output cap truncates the final message mid-sentence",
			stdout: ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd(stale, 17, 8) +
				ompToolResultEnd() +
				ompAssistantEndStopping("The review found three issues. The first is th", "length", 6, 4000) +
				ompAgentEnd(),
			wantInput: 23, wantOutput: 4008,
			why: "half a sentence is not the answer, and it is the LAST thing the run said",
		},
		{
			name: "output cap truncates a tool call, leaving no text at all",
			stdout: ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd(stale, 17, 8) +
				ompToolCallAssistantEndStopping("length") +
				ompAbortedToolResultEnd("length", "tool call truncated by the output cap") +
				ompAgentEnd(),
			wantInput: 25, wantOutput: 11,
			why: "the cap cut the tool call itself, so the loop ended with nothing said",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := OmpAdapter{
				Runner: &fakeRunner{results: []subprocess.Result{{Stdout: tc.stdout}}},
				Dir:    "/repo",
			}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
			if err == nil {
				t.Fatalf("Deliver reported a truncated run as success (%s): %+v", tc.why, result)
			}
			if result.Summary != "" {
				t.Fatalf("Summary = %q: %s — a sentence written BEFORE the cut is never the job's answer", result.Summary, tc.why)
			}
			if strings.Contains(err.Error(), stale) {
				t.Fatalf("error %q hands back the stale mid-run sentence as the cause", err.Error())
			}
			if !strings.Contains(err.Error(), "TRUNCATED") {
				t.Fatalf("error %q must NAME truncation: an operator reading it has to see the run was cut, not that it misbehaved", err.Error())
			}
			// A truncated run burned every token it burned. This is the failure mode
			// --max-time creates on the most expensive jobs, so reporting 0/0 here would
			// hide exactly the spend worth auditing.
			if result.InputTokens != tc.wantInput || result.OutputTokens != tc.wantOutput {
				t.Fatalf("usage = %d/%d, want %d/%d: a truncated run is still billed",
					result.InputTokens, result.OutputTokens, tc.wantInput, tc.wantOutput)
			}
			if result.Raw != tc.stdout {
				t.Fatalf("Raw = %q, want the stdout evidence", result.Raw)
			}
		})
	}

	// The complement, so the rule above cannot be satisfied by failing every
	// tool-using run: a run that called tools and THEN answered is a success, and its
	// answer is the final message's text.
	t.Run("a run that answers after its tool loop still succeeds", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompTextThenToolCallAssistantEnd(stale, "toolUse", 17, 8) +
			ompToolResultEnd() +
			ompAssistantEnd("no blocking issues", 6, 4) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err != nil {
			t.Fatalf("Deliver failed a run that finished its tool loop and answered: %v", err)
		}
		if result.Summary != "no blocking issues" {
			t.Fatalf("Summary = %q, want the FINAL assistant text", result.Summary)
		}
	})
}

// TestOmpDeliverUsageSums proves usage is SUMMED across assistant messages and
// read from the input/output fields. Kills: last-event-wins (would report 7/3) and
// the input_tokens/output_tokens spelling (would report 0/0 for every omp job).
func TestOmpDeliverUsageSums(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("partial", 10, 5) +
		ompToolResultEnd() +
		ompAssistantEnd("done", 7, 3) +
		ompAgentEnd()
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.InputTokens != 17 || result.OutputTokens != 8 {
		t.Fatalf("usage = %d/%d, want 17/8 (10+7 in, 5+3 out; the toolResult event must not count)",
			result.InputTokens, result.OutputTokens)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want the LAST assistant text", result.Summary)
	}
	if result.CumulativeUsage {
		t.Fatal("CumulativeUsage = true; omp reports per-run usage, never session-cumulative")
	}
	// SessionDiag on the SUCCESS path, not just the failure paths: workflow/mailbox.go
	// feeds it to storeFailureDiagnostics for a job that succeeded at the adapter but
	// produced no extractable envelope — exactly the case where the exit code, stderr
	// and session id are the only evidence left. Kills: SessionDiag=nil on success.
	if result.SessionDiag == nil {
		t.Fatal("SessionDiag = nil on the happy path; a no-envelope job would have no process evidence at all")
	}
	if result.SessionDiag.SessionID != ompFixtureSessionID {
		t.Fatalf("SessionDiag.SessionID = %q, want the header id %q", result.SessionDiag.SessionID, ompFixtureSessionID)
	}
}

// TestOmpDeliverUsageAbsentDegradesToZero kills a parser that errors (or panics)
// when a provider omits the usage block: missing usage under-counts the token
// budget, it does not fail the job.
func TestOmpDeliverUsageAbsentDegradesToZero(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEndNoUsage("done") + ompAgentEnd()
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("usage = %d/%d, want 0/0 when the stream reports none", result.InputTokens, result.OutputTokens)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want %q", result.Summary, "done")
	}
}

// TestOmpDeliverTelemetryRollupReplacesSum: agent_end.telemetry (OTEL-only, and
// the other usage spelling) is the run's own rollup of the same numbers. Kills:
// adding it to the per-message sum, which double-counts every job on an
// OTEL-configured host.
func TestOmpDeliverTelemetryRollupReplacesSum(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("partial", 10, 5) +
		ompAssistantEnd("done", 7, 3) +
		ompAgentEndWithTelemetry(100, 40)
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.InputTokens != 100 || result.OutputTokens != 40 {
		t.Fatalf("usage = %d/%d, want the telemetry rollup 100/40 (not the 17/8 sum, not 117/48)",
			result.InputTokens, result.OutputTokens)
	}
}

// TestOmpDeliverTelemetryRollupsSumAcrossSegments: the rollup is per-agentLoop
// INVOCATION, not per run — a retry or continuation starts a new collector — so a
// run with two agent_end events carries two disjoint rollups. Kills: last-wins,
// which would book only the final segment and undercount every multi-segment job
// on an OTEL host.
func TestOmpDeliverTelemetryRollupsSumAcrossSegments(t *testing.T) {
	t.Run("every segment instrumented sums the rollups", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("partial", 10, 5) +
			ompAgentEndTelemetryContinuation(1000, 400) +
			ompAssistantEnd("done", 7, 3) +
			ompAgentEndWithTelemetry(30, 10)
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if result.InputTokens != 1030 || result.OutputTokens != 410 {
			t.Fatalf("usage = %d/%d, want the summed rollups 1030/410 (not the last segment's 30/10, not the 17/8 message sum)",
				result.InputTokens, result.OutputTokens)
		}
	})

	t.Run("partially instrumented falls back to the per-message sum", func(t *testing.T) {
		// Only the second invocation reported a rollup, so the rollup total covers
		// only part of the run; the per-message sum covers all of it.
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("partial", 10, 5) +
			ompAgentEndContinuation() +
			ompAssistantEnd("done", 7, 3) +
			ompAgentEndWithTelemetry(30, 10)
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if result.InputTokens != 17 || result.OutputTokens != 8 {
			t.Fatalf("usage = %d/%d, want the per-message sum 17/8 when only some agent_end events carried a rollup",
				result.InputTokens, result.OutputTokens)
		}
	})

	// Counting agent_end events cannot prove the rollup covers the run: omp parks
	// intra-prompt agent_end events in a single-slot #pendingAgentEndEmit and each
	// later one SUPERSEDES the previous (agent-session.ts:1966-1969), while
	// message_end is always written (agent-session.ts:2354-2360). So a run whose
	// earlier invocation's agent_end was swallowed still reads as fully instrumented
	// (agentEndCount == rollupCount == 1) with a whole segment's usage missing.
	// The per-message sum covers every assistant message and is therefore a LOWER
	// BOUND on the bill. Kills: preferring a rollup that is below it, which
	// under-reports exactly the run that spent the most.
	t.Run("a rollup below the per-message sum loses to the sum", func(t *testing.T) {
		cases := []struct {
			name                      string
			rollupInput, rollupOutput int
			wantInput, wantOutput     int
			why                       string
		}{
			{
				name: "both axes below", rollupInput: 30, rollupOutput: 10,
				wantInput: 1030, wantOutput: 410,
				why: "a swallowed segment leaves the final invocation's rollup far below what was billed",
			},
			{
				name: "only the input axis below", rollupInput: 30, rollupOutput: 4000,
				wantInput: 1030, wantOutput: 410,
				why: "an undercount on either axis disqualifies the rollup, so the sum wins on both",
			},
			{
				name: "only the output axis below", rollupInput: 4000, rollupOutput: 10,
				wantInput: 1030, wantOutput: 410,
				why: "an undercount on either axis disqualifies the rollup, so the sum wins on both",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stdout := ompHeaderLine(ompFixtureSessionID) +
					ompAssistantEnd("partial", 1000, 400) +
					ompAssistantEnd("done", 30, 10) +
					ompAgentEndWithTelemetry(tc.rollupInput, tc.rollupOutput)
				runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
				adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
				result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
				if err != nil {
					t.Fatalf("Deliver: %v", err)
				}
				if result.InputTokens != tc.wantInput || result.OutputTokens != tc.wantOutput {
					t.Fatalf("usage = %d/%d, want the per-message sum %d/%d (rollup was %d/%d): %s",
						result.InputTokens, result.OutputTokens, tc.wantInput, tc.wantOutput,
						tc.rollupInput, tc.rollupOutput, tc.why)
				}
			})
		}
	})

	// EQUALITY is the normal OTEL shape, not an edge case: the rollup counts the same
	// prompt tokens the per-message blocks do, so a fully instrumented single-segment
	// run lands exactly ON the lower bound on the input axis while the rollup's extra
	// fidelity shows up on output (and vice versa). The comparison is therefore >=,
	// not >. Kills: tightening either axis to a strict >, which throws away the
	// rollup on the most ordinary instrumented run there is.
	t.Run("a rollup EQUAL to the sum on one axis still wins", func(t *testing.T) {
		cases := []struct {
			name                      string
			rollupInput, rollupOutput int
			why                       string
		}{
			{
				name: "equal on input, above on output", rollupInput: 1030, rollupOutput: 500,
				why: "the rollup sees the same prompt tokens as the messages and more output than they report",
			},
			{
				name: "above on input, equal on output", rollupInput: 1200, rollupOutput: 410,
				why: "the rollup adds input the messages cannot see and agrees on output",
			},
			{
				name: "equal on both axes", rollupInput: 1030, rollupOutput: 410,
				why: "a fully instrumented run with nothing extra to add must still take the rollup",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stdout := ompHeaderLine(ompFixtureSessionID) +
					ompAssistantEnd("partial", 1000, 400) +
					ompAssistantEnd("done", 30, 10) +
					ompAgentEndWithTelemetry(tc.rollupInput, tc.rollupOutput)
				runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
				adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
				result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
				if err != nil {
					t.Fatalf("Deliver: %v", err)
				}
				if result.InputTokens != tc.rollupInput || result.OutputTokens != tc.rollupOutput {
					t.Fatalf("usage = %d/%d, want the rollup %d/%d (per-message sum is 1030/410): %s",
						result.InputTokens, result.OutputTokens, tc.rollupInput, tc.rollupOutput, tc.why)
				}
			})
		}
	})
}

// TestOmpDeliverOversizeAgentEndLineParses is the line-length guard. omp's
// terminal agent_end re-serializes the ENTIRE run transcript onto ONE line, so the
// line this parser REQUIRES is structurally the largest in the stream and grows
// with every tool result. Kills: a fixed per-line ceiling (the 1 MiB
// bufio.Scanner the sibling adapters use), which would fail a job that actually
// succeeded and book 0/0 tokens for it.
func TestOmpDeliverOversizeAgentEndLineParses(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("done", 10, 5) +
		ompAgentEndWithTranscript(1200*1024)
	if len(stdout) <= 1024*1024 {
		t.Fatalf("fixture is only %d bytes; it must exceed the 1 MiB ceiling it exists to kill", len(stdout))
	}

	t.Run("parse", func(t *testing.T) {
		text, sessionID, usage, err := parseOmpStreamJSON(stdout)
		if err != nil {
			t.Fatalf("parseOmpStreamJSON rejected a >1 MiB agent_end line: %v", err)
		}
		if text != "done" {
			t.Fatalf("text = %q, want %q", text, "done")
		}
		if sessionID != ompFixtureSessionID {
			t.Fatalf("sessionID = %q, want %q", sessionID, ompFixtureSessionID)
		}
		if usage.InputTokens != 10 || usage.OutputTokens != 5 {
			t.Fatalf("usage = %d/%d, want 10/5 — a line-length failure must never zero the usage",
				usage.InputTokens, usage.OutputTokens)
		}
	})

	t.Run("deliver", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "implement it"})
		if err != nil {
			t.Fatalf("Deliver failed a successful run because of its transcript size: %v", err)
		}
		if result.Summary != "done" {
			t.Fatalf("Summary = %q, want %q", result.Summary, "done")
		}
		if result.InputTokens != 10 || result.OutputTokens != 5 {
			t.Fatalf("usage = %d/%d, want 10/5", result.InputTokens, result.OutputTokens)
		}
	})

	t.Run("oversize tool_execution_end mid-stream", func(t *testing.T) {
		// tool_execution_end carries `result: any` verbatim, and omp's own read tool
		// permits a 2 MiB single result — one such line is double the old ceiling.
		big := ompHeaderLine(ompFixtureSessionID) +
			fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":{"text":%s}}`,
				ompJSONString(strings.Repeat("y", 1100*1024))) + "\n" +
			ompAssistantEnd("done", 4, 2) +
			ompAgentEnd()
		text, _, usage, err := parseOmpStreamJSON(big)
		if err != nil {
			t.Fatalf("parseOmpStreamJSON rejected a >1 MiB tool_execution_end line: %v", err)
		}
		if text != "done" || usage.InputTokens != 4 || usage.OutputTokens != 2 {
			t.Fatalf("text/usage = %q %d/%d, want \"done\" 4/2", text, usage.InputTokens, usage.OutputTokens)
		}
	})
}

// TestOmpDeliverRecoveredRetryIsNotAFailure: omp absorbs transient provider
// failures itself, and the failed attempt AND the successful one land on the SAME
// stream — every event but agent_end is emitted eagerly (agent-session.ts:2354-2360,
// pushed onto the stream at agent-loop.ts:941/:1201), and the retry decision is
// taken AFTER that message_end (turn-recovery.ts:84 is what counts as a retryable
// failed tail; :226 is the successful settle that closes the saga). Kills: latching
// the first error, which fails a completed job, throws away its answer and its
// usage, and (for a recovered usage-limit 429) sends operators to fix a credential
// that works — as well as keeping that latch ACROSS a recovery, which blames the
// wrong failure when a recovered run later dies of something else.
func TestOmpDeliverRecoveredRetryIsNotAFailure(t *testing.T) {
	t.Run("assistant recovers after the retry", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAgentEndContinuation() +
			ompAutoRetryStart(1, "Overloaded") +
			ompAssistantEnd("no blocking issues", 10, 5) +
			ompAutoRetryEnd(true, 1) +
			ompAgentEnd()
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err != nil {
			t.Fatalf("Deliver failed a run omp itself recovered: %v", err)
		}
		if result.Summary != "no blocking issues" {
			t.Fatalf("Summary = %q, want the recovered answer", result.Summary)
		}
		// The failed attempt was billed too (9/0), so both segments count.
		if result.InputTokens != 19 || result.OutputTokens != 5 {
			t.Fatalf("usage = %d/%d, want 19/5 (the failed attempt is billed as well)",
				result.InputTokens, result.OutputTokens)
		}
	})

	t.Run("a recovery that begins with a tool call succeeds end-to-end", func(t *testing.T) {
		// The recovered turn's first act is a tool call, and only THEN does it answer.
		// This pins the end-to-end shape only; it cannot isolate the toolCall clause
		// of ompAssistantCarriesRealContent (the later text answer would clear the
		// failure too) — that clause's mutant is killed by
		// TestOmpAssistantContentClassifierMirrorsOmp/tool_call_only.
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAutoRetryStart(1, "Overloaded") +
			ompToolCallAssistantEnd() +
			ompAutoRetryEnd(true, 1) +
			`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":{"ok":true}}` + "\n" +
			ompAssistantEnd("no blocking issues", 4, 2) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err != nil {
			t.Fatalf("Deliver failed a run that recovered through a tool call: %v", err)
		}
		if result.Summary != "no blocking issues" {
			t.Fatalf("Summary = %q, want the recovered answer", result.Summary)
		}
	})

	// omp's own emitter cannot produce "auto_retry_end{success:true} is the only
	// recovery signal": onAssistantSettledSuccessfully (turn-recovery.ts:224-249)
	// returns early unless the message is a settled, non-empty-stop assistant message
	// (:226-231), and that message_end is already on the wire — non-agent_end events
	// are emitted eagerly (agent-session.ts:2354-2360) while the settle callback runs
	// later (agent-session.ts:2477). So this fixture is drift, and the adapter must
	// fail CLOSED on it. Kills: restoring the `success:true ⇒ turnFailed = false`
	// clause, which would answer a failed run with the sentence it wrote BEFORE the
	// failure.
	t.Run("auto_retry_end success alone does not clear the failure", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("the diff looks fine", 6, 4) +
			ompFailedAssistantEnd("error", "stream stalled", 500) +
			ompAutoRetryEnd(true, 1) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver reported a success verdict with no recovered content as success: %+v", result)
		}
		if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "stream stalled") {
			t.Fatalf("error %q must still name the unrecovered failure", err.Error())
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q: the pre-failure sentence must never become the job's answer", result.Summary)
		}
	})

	t.Run("recovered usage limit is not an auth failure", func(t *testing.T) {
		// A 429 whose provider text names the api key: latching it would ALSO trip
		// the auth classifier and tell operators to fix a working credential.
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "usage limit reached for this api key, retry after 60s", 429) +
			ompAgentEndContinuation() +
			ompAutoRetryStart(1, "usage limit reached") +
			ompAssistantEnd("done", 1, 1) +
			ompAutoRetryEnd(true, 1) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err != nil {
			t.Fatalf("Deliver failed a recovered usage-limit run: %v", err)
		}
		if result.Summary != "done" {
			t.Fatalf("Summary = %q, want the recovered answer", result.Summary)
		}
	})

	// A RECOVERED saga's error is not the run's cause. Kills: keeping the first-error
	// latch across a recovery (`firstErr` surviving the clear), which reports the blip
	// omp already absorbed instead of the failure that actually ended the job.
	t.Run("a recovered blip is not the cause of a later failure", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAgentEndContinuation() +
			ompAutoRetryStart(1, "Overloaded") +
			ompAssistantEnd("let me look at the diff", 10, 5) +
			ompAutoRetryEnd(true, 1) +
			ompFailedAssistantEnd("error", "context length exceeded: 210000 > 200000 tokens", 400) +
			ompAgentEnd()
		_, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatal("Deliver succeeded on a run whose last assistant turn failed")
		}
		if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "context length exceeded") {
			t.Fatalf("error %q must report the terminal 400 that actually killed the job", err.Error())
		}
		if strings.Contains(err.Error(), "529") || strings.Contains(err.Error(), "Overloaded") {
			t.Fatalf("error %q reports a blip omp itself recovered from", err.Error())
		}
	})

	// The compounding half of the same latch: the recovered blip's text names the api
	// key, so a stale first error would ALSO drive isOmpAuthFailure and send operators
	// to replace a credential that works.
	t.Run("a recovered api-key 429 does not misclassify a later failure as auth", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "usage limit reached for this api key, retry after 60s", 429) +
			ompAutoRetryStart(1, "usage limit reached") +
			ompAssistantEnd("let me look at the diff", 10, 5) +
			ompAutoRetryEnd(true, 1) +
			ompFailedAssistantEnd("error", "context length exceeded: 210000 > 200000 tokens", 400) +
			ompAgentEnd()
		_, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatal("Deliver succeeded on a run whose last assistant turn failed")
		}
		if strings.Contains(err.Error(), OmpAuthSetupMessage) || strings.Contains(err.Error(), "authentication required") {
			t.Fatalf("error %q blames a credential that worked: the 429 was recovered", err.Error())
		}
		if !strings.Contains(err.Error(), "400") {
			t.Fatalf("error %q must report the terminal failure", err.Error())
		}
	})

	// omp's REAL giving-up order: the failed message_end comes first and
	// auto_retry_end{success:false} closes the saga after it (turn-recovery.ts:257-261
	// `onErrorSettledWithoutRetry`, :527-541 empty-stop cap, :1462-1470 budget
	// exhausted, :1490-1495 classifier refusal), then the run settles terminally. The
	// earlier successful assistant message is load-bearing: it is the stale text a
	// wrongly-cleared failure would hand back as the job's answer. Kills:
	// verdict-blind clearing (`if event.Success != nil` treating ANY auto_retry_end
	// as recovery), which would report this giving-up saga as success with the stale
	// text as the answer. Mutants that widen failure (dropping `&& !*event.Success`
	// so any verdict fails, or dropping the success:false handling) are killed by
	// the recovered-retry and contentless-turn tests — a test expecting failure
	// structurally cannot catch them.
	t.Run("a retry that never recovers still fails", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Let me start by reading the diff.", 5, 2) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAutoRetryEnd(false, 10) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver succeeded on a retry saga that gave up: %+v", result)
		}
		if !strings.Contains(err.Error(), "529") || !strings.Contains(err.Error(), "Overloaded") {
			t.Fatalf("error %q must carry the provider failure the saga gave up on", err.Error())
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q: an intermediate sentence must never answer a run that gave up", result.Summary)
		}
	})
}

// TestOmpDeliverContentlessAssistantTurnDoesNotClearAFailure closes the hole a
// non-error stopReason opens: omp's failure classifier is WIDER than error/aborted.
// #isEmptyAssistantStop (turn-recovery.ts:554-576) counts a "stop" with no
// text/toolCall and a "toolUse" with no toolCall as failures, agent-session.ts:
// 2685-2688 routes them into bounded retries, and the cap closes the saga with
// auto_retry_end{success:false} (turn-recovery.ts:527-541). Every message on such a
// stream carries a healthy-looking stopReason, so a parser that clears a pending
// failure on ANY later assistant message_end hands back a real provider error as a
// SUCCESS whose answer is a stale mid-run sentence. Kills: clear-on-any-later-
// message_end (dropping the real-content requirement).
func TestOmpDeliverContentlessAssistantTurnDoesNotClearAFailure(t *testing.T) {
	const stale = "Reading the files now."
	shapes := []struct {
		name  string
		event string
	}{
		{name: "stop with no content at all", event: ompEmptyAssistantEnd("stop")},
		{name: "stop with whitespace-only text", event: ompWhitespaceAssistantEnd("stop")},
		{name: "toolUse with no toolCall", event: ompEmptyAssistantEnd("toolUse")},
		{name: "toolUse with whitespace-only text", event: ompWhitespaceAssistantEnd("toolUse")},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			stdout := ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd(stale, 50, 10) +
				ompFailedAssistantEnd("error", "Overloaded", 529) +
				ompAutoRetryStart(1, "Overloaded") +
				shape.event +
				ompAutoRetryEndFinal(1, "Assistant returned empty stop after retry cap; try switching models") +
				ompAgentEnd()
			result, err := OmpAdapter{
				Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
				Dir:    "/repo",
			}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
			if err == nil {
				t.Fatalf("Deliver reported a content-less turn as recovery: %+v", result)
			}
			// The 529 is the run's cause: the empty stop neither cleared it nor
			// replaced it with the saga's closing finalError.
			if !strings.Contains(err.Error(), "529") || !strings.Contains(err.Error(), "Overloaded") {
				t.Fatalf("error %q must still name the provider failure the empty stop tried to erase", err.Error())
			}
			if result.Summary != "" {
				t.Fatalf("Summary = %q: the mid-run sentence %q must never become the job's answer", result.Summary, stale)
			}
		})
	}

	// Below the cap omp retries an empty stop without closing anything, so a stream
	// can carry several of them and no verdict at all. None of them is recovery.
	t.Run("repeated empty stops with no closing verdict do not clear the failure", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Let me read the diff first.", 12, 3) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompEmptyAssistantEnd("stop") +
			ompEmptyAssistantEnd("stop") +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver reported repeated empty stops as recovery: %+v", result)
		}
		if !strings.Contains(err.Error(), "529") {
			t.Fatalf("error %q must still name the provider failure", err.Error())
		}
	})

	// The empty-stop CAP with no provider error anywhere: every stopReason on this
	// wire is "stop", so omp's own auto_retry_end{success:false} is the entire failure
	// signal and its finalError is the entire diagnosis. Kills: ignoring the
	// success:false verdict, which reports this run as a success answering with the
	// sentence from before the empty stops.
	t.Run("empty-stop cap with no provider error fails on the verdict alone", func(t *testing.T) {
		const finalError = "Assistant returned empty stop after retry cap; try switching models or `/shake images` to remove archived frames"
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Let me read the diff first.", 12, 3) +
			ompEmptyAssistantEnd("stop") +
			ompEmptyAssistantEnd("stop") +
			ompEmptyAssistantEnd("stop") +
			ompAutoRetryEndFinal(3, finalError) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver reported an empty-stop cap as success: %+v", result)
		}
		if !strings.Contains(err.Error(), finalError) {
			t.Fatalf("error %q must carry omp's own finalError — it is the only diagnosis on this wire", err.Error())
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want no answer at all from a capped run", result.Summary)
		}
	})

	// A verdict with no finalError still fails, and says so rather than reporting a
	// nil cause. Kills: setting the failure flag without a cause, which returns
	// err=nil with an empty Summary — a silent empty success.
	t.Run("a giving-up verdict with no finalError still fails loudly", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Let me read the diff first.", 12, 3) +
			ompEmptyAssistantEnd("stop") +
			ompAutoRetryEnd(false, 3) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver reported a verdict-only failure as success: %+v", result)
		}
		if !strings.Contains(err.Error(), "gave up retrying") {
			t.Fatalf("error %q must name omp's giving-up verdict as the cause", err.Error())
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want no answer at all", result.Summary)
		}
	})
}

// TestOmpDeliverGiveUpVerdictSurvivesLaterContent pins the other direction of the
// same rule: auto_retry_end{success:false} SETS a failure (amendment 1, condition
// 1(a)), and a SET that a later message can silently un-set is not a rule at all.
// omp announced it was giving up; a content-carrying assistant message that arrives
// afterwards does not overturn the runtime's own verdict, and the sentence it would
// hand back is the same stale text every other rule in this section refuses.
//
// LATENT against omp v17.2.4, deliberately: every success:false emitter in
// turn-recovery.ts (:257-261, :527-541, :1462-1470, :1490-1495, :1512-1520, :1541)
// is immediately followed by `return false` and a settled turn, so no post-give-up
// assistant message_end can reach the parser today. The guard exists so a future
// emitter ordering cannot quietly convert a given-up run into a success — which is
// exactly what this fixture asks it to do. Kills: dropping the gaveUp latch from
// the recovery clause.
func TestOmpDeliverGiveUpVerdictSurvivesLaterContent(t *testing.T) {
	const finalError = "gave up after 10 attempts: upstream is overloaded"
	const late = "here is your answer"
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAutoRetryStart(1, "Overloaded") +
		ompAutoRetryEndFinal(10, finalError) +
		ompAssistantEnd(late, 12, 6) +
		ompAgentEnd()
	result, err := OmpAdapter{
		Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
		Dir:    "/repo",
	}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if err == nil {
		t.Fatalf("Deliver let a later message overturn omp's own giving-up verdict: %+v", result)
	}
	if !strings.Contains(err.Error(), finalError) {
		t.Fatalf("error %q must carry omp's finalError: it is the run's cause", err.Error())
	}
	if result.Summary != "" {
		t.Fatalf("Summary = %q: a run omp declared lost has no answer", result.Summary)
	}
	if strings.Contains(err.Error(), late) {
		t.Fatalf("error %q reports the post-verdict message as the cause", err.Error())
	}
	// Non-vacuity: the very same late message DOES clear an ordinary provider failure
	// (that is TestOmpDeliverRecoveredRetryIsNotAFailure's contract). If this half
	// ever fails, the fixture stopped being a recovery-shaped message and the test
	// above proves nothing about the latch.
	recovered := ompHeaderLine(ompFixtureSessionID) +
		ompFailedAssistantEnd("error", "Overloaded", 529) +
		ompAutoRetryStart(1, "Overloaded") +
		ompAssistantEnd(late, 12, 6) +
		ompAutoRetryEnd(true, 1) +
		ompAgentEnd()
	okResult, okErr := OmpAdapter{
		Runner: &fakeRunner{results: []subprocess.Result{{Stdout: recovered}}},
		Dir:    "/repo",
	}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if okErr != nil || okResult.Summary != late {
		t.Fatalf("the same message must still clear an ordinary failure: err=%v summary=%q", okErr, okResult.Summary)
	}
}

// TestOmpAssistantContentClassifierMirrorsOmp pins the recovery predicate directly,
// because in a well-formed stream a later text message would clear the failure
// anyway and hide which clause did the work. Kills: dropping the toolCall clause
// (a recovered tool-calling turn read as still-failed) and dropping the
// whitespace trim (omp uses hasNonWhitespace, /\S/, at turn-recovery.ts:554-576).
func TestOmpAssistantContentClassifierMirrorsOmp(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
		why     string
	}{
		{name: "text", content: `[{"type":"text","text":"done"}]`, want: true,
			why: "a non-whitespace text part is the ordinary recovery"},
		{name: "tool call only", content: `[{"type":"toolCall","toolCallId":"c1","toolName":"read"}]`, want: true,
			why: "a toolCall makes both a stop and a toolUse non-empty for omp"},
		{name: "tool call after empty text", content: `[{"type":"text","text":"   "},{"type":"toolCall","toolCallId":"c1"}]`, want: true,
			why: "one real part anywhere in the content is enough"},
		{name: "no content", content: `[]`, want: false, why: "omp's #isEmptyAssistantStop shape"},
		{name: "whitespace text", content: `[{"type":"text","text":" \n\t "}]`, want: false,
			why: "hasNonWhitespace(/\\S/) rejects it, so omp counts it empty"},
		{name: "thinking only", content: `[{"type":"thinking","thinking":"stalling","thinkingSignature":"sig"}]`, want: false,
			why: "a signature can never be the job's answer, so it cannot clear a failure here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var message ompStreamMessage
			if err := json.Unmarshal([]byte(`{"role":"assistant","stopReason":"stop","content":`+tc.content+`}`), &message); err != nil {
				t.Fatalf("fixture does not decode: %v", err)
			}
			if got := ompAssistantCarriesRealContent(&message); got != tc.want {
				t.Fatalf("ompAssistantCarriesRealContent = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestOmpDeliverPendingRetryAtStreamEndIsError: a retry saga that is still OPEN when
// the stream ends is a failure, never "the error was being handled". omp closes
// every saga it opens — on success from onAssistantSettledSuccessfully
// (turn-recovery.ts:224-249) and on every dead end from :257-261, :527-541,
// :1462-1470, :1490-1495, whose comment at :1481-1487 states the invariant outright
// ("the saga must close with its own auto_retry_end … so subscribers tracking
// retry-outstanding state don't stay latched on an announcement that never
// resolves"). So an auto_retry_start with nothing after it means the process died
// mid-retry. Kills: treating a trailing auto_retry_start as benign/ignorable, which
// answers a run that was mid-retry with whatever it had said before.
func TestOmpDeliverPendingRetryAtStreamEndIsError(t *testing.T) {
	// The terminal agent_end sits BEFORE the dangling auto_retry_start on purpose:
	// every other guard is satisfied (terminal settle present, no failed turn,
	// non-empty text), so only the pending-retry guard can fail this stream. Like
	// ompAgentEndContinuation, this is the shape the adapter must survive, not one
	// omp is claimed to emit.
	t.Run("an open saga after a terminal settle", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Reading the files now.", 5, 2) +
			ompAgentEnd() +
			ompAutoRetryStart(1, "Overloaded")
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver answered a run whose retry never resolved: %+v", result)
		}
		if !strings.Contains(err.Error(), "auto_retry_start") || !strings.Contains(err.Error(), "mid-retry") {
			t.Fatalf("error %q must name the unresolved retry saga", err.Error())
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q: a pending retry must not answer with what came before it", result.Summary)
		}
	})

	// The literal "process died mid-retry" wire: the failed turn, the retry
	// announcement, then nothing. Two independent guards condemn it (the failure is
	// still standing AND the saga is open), so this asserts only that it fails —
	// which guard wins is not the contract.
	t.Run("process died between the announcement and the retry", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Reading the files now.", 5, 2) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAutoRetryStart(1, "Overloaded")
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver answered a stream cut mid-retry: %+v", result)
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want no answer from a stream cut mid-retry", result.Summary)
		}
	})

	// A closed saga is not pending: the guard must not fire on the ordinary recovered
	// run. (Kills a guard that latches on auto_retry_start and never clears.)
	t.Run("a closed saga does not trip the guard", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "Overloaded", 529) +
			ompAutoRetryStart(1, "Overloaded") +
			ompAssistantEnd("no blocking issues", 10, 5) +
			ompAutoRetryEnd(true, 1) +
			ompAgentEnd()
		result, err := OmpAdapter{
			Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
			Dir:    "/repo",
		}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err != nil {
			t.Fatalf("Deliver failed a run whose retry saga closed normally: %v", err)
		}
		if result.Summary != "no blocking issues" {
			t.Fatalf("Summary = %q, want the recovered answer", result.Summary)
		}
	})
}

// TestOmpDeliverFirstStreamErrorWins: when a run really does end failed, the
// FIRST failure is the reported one — a later failure in the same run is usually a
// cascade of it. Kills: last-wins, which would report the downstream symptom.
func TestOmpDeliverFirstStreamErrorWins(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompFailedAssistantEnd("error", "first failure: upstream is overloaded", 529) +
		ompAutoRetryStart(1, "first failure: upstream is overloaded") +
		ompFailedAssistantEnd("error", "second failure: gateway timeout", 504) +
		ompAgentEnd()
	_, err := OmpAdapter{
		Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}},
		Dir:    "/repo",
	}.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if err == nil {
		t.Fatal("Deliver succeeded on a run whose every assistant turn failed")
	}
	if !strings.Contains(err.Error(), "first failure") || !strings.Contains(err.Error(), "529") {
		t.Fatalf("error %q must report the FIRST failure", err.Error())
	}
	if strings.Contains(err.Error(), "second failure") || strings.Contains(err.Error(), "504") {
		t.Fatalf("error %q reported the cascading second failure", err.Error())
	}
}

// TestParseOmpStreamJSONRequiresTerminalAgentEnd: an agent_end is tagged
// isTerminal:false whenever it was emitted with willContinue, so "an agent_end
// arrived" does not mean "the run finished" — the last one has to be the final
// settle. (On omp's normal print-mode wire the non-terminal ones are superseded
// before they reach stdout, per ompAgentEndContinuation; this guard is what makes a
// stream that DOES end on one fail rather than answer.) Kills: accepting a stream
// truncated between attempts, which reports an intermediate sentence as the job's
// answer.
func TestParseOmpStreamJSONRequiresTerminalAgentEnd(t *testing.T) {
	truncated := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("Let me start by reading the diff.", 5, 2) +
		ompAgentEndContinuation() +
		`{"type":"auto_compaction_start","reason":"context"}` + "\n"

	t.Run("parse", func(t *testing.T) {
		text, sessionID, _, err := parseOmpStreamJSON(truncated)
		if err == nil {
			t.Fatalf("parseOmpStreamJSON accepted a stream truncated mid-continuation (text=%q)", text)
		}
		if !strings.Contains(err.Error(), "isTerminal") {
			t.Fatalf("error %q must name the non-terminal agent_end", err.Error())
		}
		if sessionID != ompFixtureSessionID {
			t.Fatalf("sessionID = %q, want the header id even on the failure path", sessionID)
		}
	})

	t.Run("deliver", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: truncated}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatalf("Deliver reported a truncated continuation as success: %+v", result)
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want no answer at all from a truncated run", result.Summary)
		}
		if result.Raw != truncated {
			t.Fatalf("Raw = %q, want the stdout evidence", result.Raw)
		}
	})

	t.Run("an absent isTerminal field still reads as terminal", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd("done", 2, 1) + ompAgentEndUntagged()
		text, _, _, err := parseOmpStreamJSON(stdout)
		if err != nil {
			t.Fatalf("an untagged agent_end must stay terminal (forward compatibility): %v", err)
		}
		if text != "done" {
			t.Fatalf("text = %q, want %q", text, "done")
		}
	})
}

// TestOmpAssistantTextKeepsOnlyTextParts: an assistant message mixes thinking,
// tool-call and text parts. Kills: concatenating every part regardless of type,
// which would leak reasoning or tool arguments into the extracted result.
func TestOmpAssistantTextKeepsOnlyTextParts(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEndMixedParts("done") + ompAgentEnd()
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want only the text part %q", result.Summary, "done")
	}
	if strings.Contains(result.Raw, "LEAKED TOOL CALL") || strings.Contains(result.Raw, "let me stall") {
		t.Fatalf("Raw = %q leaked a non-text content part", result.Raw)
	}
}

// TestOmpSummaryTrimsAssistantText: Summary is TrimSpace(Raw). Kills: dropping the
// trim — a Summary with a leading newline is exactly what breaks fenced-block
// extraction — while Raw keeps the model's bytes untouched as evidence.
func TestOmpSummaryTrimsAssistantText(t *testing.T) {
	const text = "\n  done  \n"
	stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd(text, 1, 1) + ompAgentEnd()
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want the trimmed %q", result.Summary, "done")
	}
	if result.Raw != text {
		t.Fatalf("Raw = %q, want the assistant text verbatim %q", result.Raw, text)
	}
}

// TestParseOmpStreamJSONIgnoresUnknownEvents keeps the parser forward-compatible:
// omp emits session-level events this adapter has never heard of, and a new one
// must not break a good run.
func TestParseOmpStreamJSONIgnoresUnknownEvents(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		`{"type":"agent_start"}` + "\n" +
		`{"type":"notice","level":"warn","message":"extension slow"}` + "\n" +
		"this line is not json at all\n" +
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":{"ok":true}}` + "\n" +
		`{"type":"model_changed","model":"anthropic/claude-opus-4"}` + "\n" +
		ompAssistantEnd("done", 2, 1) +
		"\n" +
		ompAgentEnd()
	text, sessionID, usage, err := parseOmpStreamJSON(stdout)
	if err != nil {
		t.Fatalf("parseOmpStreamJSON: %v", err)
	}
	if text != "done" {
		t.Fatalf("text = %q, want %q", text, "done")
	}
	if sessionID != ompFixtureSessionID {
		t.Fatalf("sessionID = %q, want %q", sessionID, ompFixtureSessionID)
	}
	if usage.InputTokens != 2 || usage.OutputTokens != 1 {
		t.Fatalf("usage = %d/%d, want 2/1", usage.InputTokens, usage.OutputTokens)
	}
}

// TestParseOmpStreamJSONUndecodableKnownEventFailsRun pins rule 6's narrow half.
// The fixture is the one the adversarial review found: an earlier assistant turn
// that SPOKE, then a FAILED message_end the parser cannot decode (valid JSON, a
// type it reads, a usage block whose `input` drifted to a string), then a terminal
// agent_end. Every other envelope rule passes, so an unconditional "unparseable
// lines are skipped" makes this run a SUCCESS whose Summary is the earlier turn's
// work note — the stale-text false green the truncation rules exist to refuse,
// arriving through the decoder instead of through stopReason.
//
// Kills: reverting the skip to unconditional; failing without NAMING the event type
// (the operator's only lead on which part of the envelope drifted); and returning
// the pre-failure text alongside the error.
//
// The complements below are the other half of the same needle: forward
// compatibility is for rows this parser does not read, so an unknown type stays
// skipped EVEN WHEN it fails to decode, and a line that is not valid JSON at all
// stays skipped even when it looks like a known type. A fix that fails on any
// decode error would pass the first assertion and break both of those.
func TestParseOmpStreamJSONUndecodableKnownEventFailsRun(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("Let me read the file first.", 12, 3) +
		ompUndecodableMessageEnd() +
		ompAgentEnd()
	text, sessionID, usage, err := parseOmpStreamJSON(stdout)
	if err == nil {
		t.Fatalf("parseOmpStreamJSON accepted a stream whose message_end could not be decoded (text=%q)", text)
	}
	if !strings.Contains(err.Error(), "message_end") {
		t.Fatalf("error %q must NAME the undecodable event type: it is the operator's only lead on which row drifted", err.Error())
	}
	if text != "" {
		t.Fatalf("text = %q, want none: that sentence was written BEFORE the row the parser could not read, so it is not the run's answer", text)
	}
	// Diagnostics survive the failure: the parser keeps scanning past the bad row
	// instead of bailing, so the header id and the usage the decodable messages
	// prove was billed are still reported (a failed run is still billed).
	if sessionID != ompFixtureSessionID {
		t.Fatalf("sessionID = %q, want the header id %q to survive the failure", sessionID, ompFixtureSessionID)
	}
	if usage.InputTokens != 12 || usage.OutputTokens != 3 {
		t.Fatalf("usage = %d/%d, want 12/3: the tokens the decodable messages proved were billed", usage.InputTokens, usage.OutputTokens)
	}

	// Every load-bearing type, not just the one the review happened to find. The
	// loop enumerates ompLoadBearingEventTypes itself, so a type added to that set
	// without a fixture here fails instead of shipping unexercised.
	t.Run("every load-bearing event type", func(t *testing.T) {
		if len(ompUndecodableRows) != len(ompLoadBearingEventTypes) {
			t.Fatalf("ompUndecodableRows covers %d types, ompLoadBearingEventTypes has %d: every load-bearing type needs a fixture",
				len(ompUndecodableRows), len(ompLoadBearingEventTypes))
		}
		for eventType := range ompLoadBearingEventTypes {
			row, ok := ompUndecodableRows[eventType]
			if !ok {
				t.Fatalf("no undecodable fixture for load-bearing event type %q", eventType)
			}
			t.Run(eventType, func(t *testing.T) {
				stream := ompHeaderLine(ompFixtureSessionID) +
					ompAssistantEnd("stale", 1, 1) + row + ompAgentEnd()
				text, _, _, err := parseOmpStreamJSON(stream)
				if err == nil {
					t.Fatalf("parseOmpStreamJSON accepted an undecodable %s row (text=%q)", eventType, text)
				}
				if !strings.Contains(err.Error(), eventType) {
					t.Fatalf("error %q must name the undecodable event type %q", err.Error(), eventType)
				}
				if text != "" {
					t.Fatalf("text = %q, want none when a %s row could not be read", text, eventType)
				}
			})
		}
	})

	// The ROLE half of the same needle, and the false RED this rule can produce if it
	// only looks at the type. message_end is load-bearing only for ASSISTANT messages
	// (rule 2's filter); omp emits one for every user, developer and custom message of
	// a turn as well, their content is legally a bare string, and Go cannot decode a
	// string into []ompContentPart. So these rows are valid JSON of a load-bearing
	// TYPE that fail to decode while being rows this parser would never have read —
	// failing them reports a COMPLETE run as envelope drift and throws its answer
	// away, the mirror image of the stale-text false green above.
	//
	// Kills: dropping the role probe (every one of these healthy streams fails).
	t.Run("a non-assistant message_end with string content is skipped", func(t *testing.T) {
		rows := map[string]string{
			"user":                   ompStringContentMessageEnd("user", "", "review the diff on this branch"),
			"developer":              ompStringContentMessageEnd("developer", "", "<system-reminder>stay on task</system-reminder>"),
			"custom plan-mode":       ompStringContentMessageEnd("custom", "plan-mode-context", "<plan-mode>write PLAN.md before editing</plan-mode>"),
			"custom lsp late diag":   ompStringContentMessageEnd("custom", "lsp-late-diagnostic", "internal/runtime/omp.go:12:2: declared and not used: x"),
			"custom todo reminder":   ompStringContentMessageEnd("custom", "todo-error-reminder", "the todo list still has open items"),
			"custom thinking loop":   ompStringContentMessageEnd("custom", "thinking-loop-redirect", "you have been thinking in circles; act"),
			"custom goal-mode ctx":   ompStringContentMessageEnd("custom", "goal-mode-context", "the active goal is: ship the adapter"),
			"custom vibe-mode ctx":   ompStringContentMessageEnd("custom", "vibe-mode-context", "keep a todo list as you go"),
			"custom live delegation": ompStringContentMessageEnd("custom", "live-delegation", "the voice model asked for a refactor"),
		}
		var everyRow string
		for name, row := range rows {
			everyRow += row
			t.Run(name, func(t *testing.T) {
				// The fixture must really be undecodable, or this subtest silently
				// stops exercising the classifier at all.
				var event ompStreamEvent
				if err := json.Unmarshal([]byte(strings.TrimSpace(row)), &event); err == nil {
					t.Fatalf("fixture decodes cleanly and so no longer reaches rule 6: %s", row)
				}
				stdout := ompHeaderLine(ompFixtureSessionID) + row +
					ompAssistantEnd("done", 2, 1) + ompAgentEnd()
				text, sessionID, usage, err := parseOmpStreamJSON(stdout)
				if err != nil {
					t.Fatalf("parseOmpStreamJSON: %v (this message_end is a row rule 2's role filter drops unread, so rule 6 must skip it instead of failing a healthy run)", err)
				}
				if text != "done" {
					t.Fatalf("text = %q, want %q: the assistant DID answer", text, "done")
				}
				if sessionID != ompFixtureSessionID {
					t.Fatalf("sessionID = %q, want %q", sessionID, ompFixtureSessionID)
				}
				if usage.InputTokens != 2 || usage.OutputTokens != 1 {
					t.Fatalf("usage = %d/%d, want 2/1", usage.InputTokens, usage.OutputTokens)
				}
			})
		}
		// A real turn carries several of them at once (a prelude, a steer, a late
		// diagnostic), so the whole set has to pass together, not just one at a time.
		t.Run("all of them in one turn", func(t *testing.T) {
			stdout := ompHeaderLine(ompFixtureSessionID) + everyRow +
				ompAssistantEnd("done", 2, 1) + ompAgentEnd()
			text, _, _, err := parseOmpStreamJSON(stdout)
			if err != nil {
				t.Fatalf("parseOmpStreamJSON: %v (a turn's worth of non-assistant message_end rows must not fail the run)", err)
			}
			if text != "done" {
				t.Fatalf("text = %q, want %q", text, "done")
			}
		})
	})

	// The complement that keeps the role probe from becoming a way OUT of rule 6: the
	// role is the discriminator, so a row that lost the discriminator too is fatal.
	// Fail-closed, because "some other role, skip it" would be a guess read out of a
	// payload the parser just admitted it could not read.
	//
	// Kills: skipping when the role is unreadable or absent.
	t.Run("an undecodable message_end with no readable role still fails", func(t *testing.T) {
		rows := map[string]string{
			"role field absent":        ompRolelessUndecodableMessageEnd(),
			"message is not an object": ompUnreadableRoleMessageEnd(),
		}
		for name, row := range rows {
			t.Run(name, func(t *testing.T) {
				stdout := ompHeaderLine(ompFixtureSessionID) +
					ompAssistantEnd("Let me read the file first.", 12, 3) + row + ompAgentEnd()
				text, _, _, err := parseOmpStreamJSON(stdout)
				if err == nil {
					t.Fatalf("parseOmpStreamJSON accepted a message_end it could not decode AND could not attribute to a role (text=%q)", text)
				}
				if !strings.Contains(err.Error(), "message_end") {
					t.Fatalf("error %q must NAME the undecodable event type", err.Error())
				}
				if text != "" {
					t.Fatalf("text = %q, want none: it predates the row the parser could not read", text)
				}
			})
		}
	})

	// Ordering, not just outcome: the unreadable row is the report even when the
	// stream ALSO said, in a row that decoded fine, that a turn failed. The decode
	// failure is the more actionable fact (envelope drift an operator must fix) and it
	// makes the other verdict provisional — the unread row may itself have been the
	// real cause, or the recovery.
	//
	// Kills: moving the undecodable return below the turnFailed one.
	t.Run("an unreadable row outranks a failure the stream reported", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "overloaded_error", 529) +
			ompUndecodableRows["agent_end"] +
			ompAgentEnd()
		text, _, _, err := parseOmpStreamJSON(stdout)
		if err == nil {
			t.Fatalf("parseOmpStreamJSON accepted a stream with an undecodable agent_end (text=%q)", text)
		}
		if !strings.Contains(err.Error(), "could not decode") {
			t.Fatalf("error %q must report the row the parser could not read, not a verdict derived from the rows it could", err.Error())
		}
		if strings.Contains(err.Error(), "omp turn failed") {
			t.Fatalf("error %q is the stopReason verdict: a stream carrying an unreadable load-bearing row cannot report that verdict as THE cause", err.Error())
		}
	})

	// The parser keeps scanning past an unreadable row instead of bailing on it, so
	// the session id and the usage arriving AFTER it still get reported. A failed run
	// is still billed, and its id is how an operator finds the session to reconcile.
	// The bad row is FIRST here on purpose: with it last, bailing and continuing are
	// indistinguishable.
	//
	// Kills: returning immediately from the undecodable branch.
	t.Run("diagnostics survive a bad row anywhere in the stream", func(t *testing.T) {
		stdout := ompUndecodableMessageEnd() +
			ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("done", 12, 3) +
			ompAgentEnd()
		text, sessionID, usage, err := parseOmpStreamJSON(stdout)
		if err == nil {
			t.Fatalf("parseOmpStreamJSON accepted a stream whose FIRST row could not be decoded (text=%q)", text)
		}
		if text != "" {
			t.Fatalf("text = %q, want none", text)
		}
		if sessionID != ompFixtureSessionID {
			t.Fatalf("sessionID = %q, want the header id %q: the parser stopped reading at the bad row", sessionID, ompFixtureSessionID)
		}
		if usage.InputTokens != 12 || usage.OutputTokens != 3 {
			t.Fatalf("usage = %d/%d, want 12/3: the tokens billed AFTER the bad row are still billed", usage.InputTokens, usage.OutputTokens)
		}
	})

	// First-wins across undecodable rows. A second drift is usually the first one
	// again a few rows later; the earliest is where the envelope actually moved and
	// the only one an operator can start from.
	//
	// Kills: latching the LAST undecodable row instead of the first.
	t.Run("two undecodable rows report the FIRST", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompUndecodableRows["session"] +
			ompAssistantEnd("stale", 1, 1) +
			ompUndecodableRows["agent_end"] +
			ompAgentEnd()
		_, _, _, err := parseOmpStreamJSON(stdout)
		if err == nil {
			t.Fatalf("parseOmpStreamJSON accepted a stream with two undecodable load-bearing rows")
		}
		if !strings.Contains(err.Error(), "carried a session event") {
			t.Fatalf("error %q must report the FIRST unreadable row (the session header), not a later one", err.Error())
		}
		if strings.Contains(err.Error(), "agent_end") {
			t.Fatalf("error %q reports the LAST unreadable row: the first is where the envelope moved", err.Error())
		}
	})

	// The forward-compat half of rule 6, and the reason this is not "fail on any
	// decode error": omp keeps adding event types (auto_compaction_*, model_changed,
	// notice), their payloads are shapes this struct never modelled, and a run that
	// this parser read completely must not fail because a row it ignores moved.
	t.Run("an unknown type that fails to decode is still skipped", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			`{"type":"auto_compaction_start","isTerminal":"soon","telemetry":"pending"}` + "\n" +
			`{"type":"model_changed","success":"maybe","id":7}` + "\n" +
			ompAssistantEnd("done", 2, 1) + ompAgentEnd()
		text, _, usage, err := parseOmpStreamJSON(stdout)
		if err != nil {
			t.Fatalf("parseOmpStreamJSON: %v (an unknown event type must stay forward-compatible even when its payload does not decode)", err)
		}
		if text != "done" {
			t.Fatalf("text = %q, want %q", text, "done")
		}
		if usage.InputTokens != 2 || usage.OutputTokens != 1 {
			t.Fatalf("usage = %d/%d, want 2/1", usage.InputTokens, usage.OutputTokens)
		}

		// The substring identification below is a LAST RESORT for lines that are not
		// valid JSON, and it must stay one. The rule under test is PRECEDENCE: this row
		// kept its structure, so its own `type` field answers the only question rule 6
		// asks — would this parser have read the row — and a marker matched in raw text
		// must not be allowed to overrule that answer.
		//
		// The fixture is CONSTRUCTED, and this comment says so rather than implying
		// otherwise: no verified omp event nests a serialized `{"type":"message_end",...}`
		// row (checked at 06343fef4 — CompactionResult's named members are scalars,
		// though its `details`/`preserveData` are unconstrained; agent_end carries
		// AgentMessage[] whose members have a role but no per-message `type`). An
		// earlier version of this comment justified the split by an auto-compaction
		// handoff "re-serializing what it summarized"; that shape was not found, and
		// the split stands on precedence, not on any payload-shape guarantee.
		//
		// Kills: running the marker scan on every line instead of only on the ones
		// that are not valid JSON.
		t.Run("an unknown type carrying a nested assistant message_end is still skipped", func(t *testing.T) {
			row := `{"type":"auto_compaction_end","isTerminal":"soon","summarized":[` +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"stale"}]}}]}`
			// PREMISE: valid JSON (so the field-level classifier is the one that
			// applies) whose payload still fails to decode (so it reaches rule 6 at
			// all). A fixture that lost either property stops exercising the boundary.
			if !json.Valid([]byte(row)) {
				t.Fatalf("fixture is not valid JSON, so it no longer probes the valid-JSON side of the boundary: %s", row)
			}
			var event ompStreamEvent
			if err := json.Unmarshal([]byte(row), &event); err == nil {
				t.Fatalf("fixture decodes cleanly and so no longer reaches rule 6: %s", row)
			}
			stdout := ompHeaderLine(ompFixtureSessionID) + row + "\n" +
				ompAssistantEnd("done", 2, 1) + ompAgentEnd()
			text, _, _, err := parseOmpStreamJSON(stdout)
			if err != nil {
				t.Fatalf("parseOmpStreamJSON: %v (an unknown type is classified from its own `type` field, not from message rows nested inside it)", err)
			}
			if text != "done" {
				t.Fatalf("text = %q, want %q", text, "done")
			}
		})
	})

	// Garbage tolerance, and the ONE exception the g7 delta review at cb34f666
	// carved out of it.
	//
	// THIS OVERRULES the seam this subtest used to pin ("a line that is not valid
	// JSON at all is still skipped", explicitly including a TRUNCATED message_end
	// line). That was a design guess — a malformed row has no reliable type to read,
	// and the missing-agent_end rules were expected to cover whatever it cost — and a
	// probe beat it: a row cut MID-STREAM leaves the terminal agent_end intact, so
	// rule 4 never fires, and no other rule sees the row at all. A malformed FINAL
	// assistant message_end therefore just disappears and the run reports SUCCESS
	// carrying the sentence an EARLIER turn wrote. Review evidence outranks the guess,
	// so the seam is now closed for exactly the rows the fragment still IDENTIFIES: a
	// `"type"` of message_end AND a `"role"` of assistant, matched at substring level
	// so the point where the line was cut does not matter.
	//
	// Everything else still skips. A stray log line or a half-written tail carrying
	// only ONE of those markers really is not evidence about the run, and failing on
	// it would turn omp's own warnings on stdout into failed jobs.
	t.Run("a line that is not valid JSON at all", func(t *testing.T) {
		// Kills: dropping the marker scan (this stream reports the stale sentence as
		// the run's answer again).
		t.Run("a malformed assistant message_end FAILS", func(t *testing.T) {
			row := ompMalformedAssistantMessageEndLine()
			// PREMISE: the row really is syntactically malformed. A fixture that
			// became valid JSON would be exercising the decodable path instead.
			if json.Valid([]byte(strings.TrimSpace(row))) {
				t.Fatalf("fixture is valid JSON, so it no longer probes the malformed path: %s", row)
			}
			stdout := ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd("Let me read the file first.", 12, 3) + row + ompAgentEnd()
			text, sessionID, usage, err := parseOmpStreamJSON(stdout)
			if err == nil {
				t.Fatalf("parseOmpStreamJSON accepted a malformed assistant message_end (text=%q): the terminal agent_end is intact, so nothing else on this stream catches it", text)
			}
			if !strings.Contains(err.Error(), "message_end") {
				t.Fatalf("error %q must NAME the shape (a malformed assistant message_end)", err.Error())
			}
			if !strings.Contains(strings.ToLower(err.Error()), "malformed") {
				t.Fatalf("error %q must say the row was MALFORMED, not merely undecodable: the operator has to look for a cut line, not a drifted field", err.Error())
			}
			if text != "" {
				t.Fatalf("text = %q, want none: that sentence was written BEFORE the row that was cut", text)
			}
			if sessionID != ompFixtureSessionID {
				t.Fatalf("sessionID = %q, want the header id %q to survive the failure", sessionID, ompFixtureSessionID)
			}
			if usage.InputTokens != 12 || usage.OutputTokens != 3 {
				t.Fatalf("usage = %d/%d, want 12/3: a failed run is still billed", usage.InputTokens, usage.OutputTokens)
			}
		})

		// Kills: failing on ANY line that is not valid JSON. Each fragment below is
		// missing one of the two markers, and every one of them is a shape omp's real
		// stdout produces — a warning printed onto the stream, and a large line cut at
		// a point that leaves only part of its identity behind.
		t.Run("unidentifiable garbage is still skipped", func(t *testing.T) {
			fragments := map[string]string{
				"a stray log line":                            "omp: warning printed onto stdout",
				"a message_end cut before the role":           `{"type":"message_end","message":{"role":"toolRes`,
				"an assistant fragment with no type marker":   `"role":"assistant","content":[{"type":"text","text":"half a li`,
				"an agent_end transcript cut mid-message":     `{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"do`,
				"a message_end cut before the role is quoted": `{"type":"message_end","message":{"role`,
			}
			var everyFragment string
			for name, fragment := range fragments {
				everyFragment += fragment + "\n"
				t.Run(name, func(t *testing.T) {
					// PREMISE: really not valid JSON, or this is not garbage
					// tolerance being tested.
					if json.Valid([]byte(fragment)) {
						t.Fatalf("fixture is valid JSON, so it no longer probes garbage tolerance: %s", fragment)
					}
					stdout := ompHeaderLine(ompFixtureSessionID) + fragment + "\n" +
						ompAssistantEnd("done", 2, 1) + ompAgentEnd()
					text, _, usage, err := parseOmpStreamJSON(stdout)
					if err != nil {
						t.Fatalf("parseOmpStreamJSON: %v (a fragment carrying only one of the two markers is not evidence about the run and stays skipped under rule 6)", err)
					}
					if text != "done" {
						t.Fatalf("text = %q, want %q: the assistant DID answer", text, "done")
					}
					if usage.InputTokens != 2 || usage.OutputTokens != 1 {
						t.Fatalf("usage = %d/%d, want 2/1", usage.InputTokens, usage.OutputTokens)
					}
				})
			}
			// A messy stream carries several at once, so they have to pass together.
			t.Run("all of them in one stream", func(t *testing.T) {
				stdout := ompHeaderLine(ompFixtureSessionID) + everyFragment +
					ompAssistantEnd("done", 2, 1) + ompAgentEnd()
				text, _, _, err := parseOmpStreamJSON(stdout)
				if err != nil {
					t.Fatalf("parseOmpStreamJSON: %v (a stream's worth of unidentifiable garbage must not fail the run)", err)
				}
				if text != "done" {
					t.Fatalf("text = %q, want %q", text, "done")
				}
			})
		})
	})

	// The symptom the review actually reported, at the boundary that ships it: the
	// stale sentence must never reach a job's Summary, and the whole stream stays as
	// evidence so the drifted row can be read by hand.
	t.Run("Deliver never answers with the pre-failure sentence", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		result, err := (OmpAdapter{Runner: runner, Dir: "/repo"}).
			Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review this"})
		if err == nil {
			t.Fatalf("Deliver succeeded on a stream with an undecodable message_end: %+v", result)
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want none: that sentence predates the row the parser could not read", result.Summary)
		}
		if result.Raw != stdout {
			t.Fatalf("Raw = %q, want the whole stream as evidence for the envelope drift", result.Raw)
		}
		if result.InputTokens != 12 || result.OutputTokens != 3 {
			t.Fatalf("usage = %d/%d, want 12/3: a failed run is still billed", result.InputTokens, result.OutputTokens)
		}
	})
}

// TestParseOmpStreamJSONMessageEndWithoutRoleFailsRun closes the bypass the g7 delta
// review probed at cb34f666, on the DECODABLE side of rule 6. A message_end whose role
// is null, absent, or whose whole `message` member is gone DECODES — encoding/json
// zero-values Role to "" — so it never reaches the undecodable classifier at all, and
// rule 2's `Role != "assistant"` filter reads that zero value as "some other role" and
// skips the row. A FAILED assistant turn shaped that way vanishes from the stream while
// every other envelope rule still passes, and the run reports SUCCESS carrying the
// sentence an EARLIER turn wrote: the same stale-text false green rule 5 and the
// undecodable rule refuse, arriving through a zero value instead of a decode error.
//
// This OVERRULES the claim the earlier comment on ompUndecodableMessageEndWasRead made
// for this path — "there the row WAS read and its role really is absent". A probe beat
// that guess: nothing about the role was READ, it was FILLED IN, and an absent role is
// absent evidence on either path. Both now fail closed.
//
// The premise is BOUND, not assumed: every fixture is asserted to decode cleanly, to be
// a message_end, and to leave an empty Role — so a fixture that drifted into the
// undecodable path would fail here instead of quietly re-testing the rule next door
// (which passes already, and would hide the loss of this one).
//
// Kills: restoring the `message == nil || message.Role != "assistant"` skip.
func TestParseOmpStreamJSONMessageEndWithoutRoleFailsRun(t *testing.T) {
	rows := map[string]string{
		"role is null":          ompDecodableFailedMessageEndWithoutRole(`"role":null,`),
		"role field absent":     ompDecodableFailedMessageEndWithoutRole(""),
		"message member absent": ompDecodableMessageEndWithoutMessage(),
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			var event ompStreamEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(row)), &event); err != nil {
				t.Fatalf("fixture does not decode (%v), so it exercises the undecodable classifier instead of the zero-value role this test is about: %s", err, row)
			}
			if event.Type != "message_end" {
				t.Fatalf("fixture type = %q, want message_end: %s", event.Type, row)
			}
			if event.Message != nil && event.Message.Role != "" {
				t.Fatalf("fixture role = %q, want the zero value: the bypass under test is a role Go FILLED IN, not one the row carried", event.Message.Role)
			}
			stdout := ompHeaderLine(ompFixtureSessionID) +
				ompAssistantEnd("Let me read the file first.", 12, 3) + row + ompAgentEnd()
			text, sessionID, usage, err := parseOmpStreamJSON(stdout)
			if err == nil {
				t.Fatalf("parseOmpStreamJSON accepted a message_end with no role (text=%q): a failed assistant turn shaped this way resurrects the earlier turn's work note as the run's answer", text)
			}
			if !strings.Contains(err.Error(), "message_end") {
				t.Fatalf("error %q must NAME the shape (a message_end with no role): it is the operator's only lead on which row drifted", err.Error())
			}
			if !strings.Contains(err.Error(), "role") {
				t.Fatalf("error %q must say the ROLE is what is missing, not merely that some row failed", err.Error())
			}
			if text != "" {
				t.Fatalf("text = %q, want none: that sentence was written BEFORE the row this parser could not attribute", text)
			}
			if sessionID != ompFixtureSessionID {
				t.Fatalf("sessionID = %q, want the header id %q to survive the failure", sessionID, ompFixtureSessionID)
			}
			if usage.InputTokens != 12 || usage.OutputTokens != 3 {
				t.Fatalf("usage = %d/%d, want 12/3: the tokens the attributable messages proved were billed", usage.InputTokens, usage.OutputTokens)
			}
		})
	}

	// The symptom at the boundary that ships it: the stale sentence must never reach
	// a job's Summary, and a failed run is still billed.
	t.Run("Deliver never answers with the pre-failure sentence", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("Let me read the file first.", 12, 3) +
			ompDecodableFailedMessageEndWithoutRole(`"role":null,`) + ompAgentEnd()
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		result, err := (OmpAdapter{Runner: runner, Dir: "/repo"}).
			Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review this"})
		if err == nil {
			t.Fatalf("Deliver succeeded on a stream whose failed assistant message_end lost its role: %+v", result)
		}
		if result.Summary != "" {
			t.Fatalf("Summary = %q, want none: that sentence predates the row this parser could not attribute", result.Summary)
		}
		if result.Raw != stdout {
			t.Fatalf("Raw = %q, want the whole stream as evidence for the envelope drift", result.Raw)
		}
		if result.InputTokens != 12 || result.OutputTokens != 3 {
			t.Fatalf("usage = %d/%d, want 12/3: a failed run is still billed", result.InputTokens, result.OutputTokens)
		}
	})

	// First-wins ACROSS shapes, which is the ONLY way to pin the roleless arm's own
	// latch guard. Two roleless rows produce the same sentence, so a stream carrying
	// two of them cannot tell "keep the first" from "overwrite with the last" — the
	// same-shape subtest next door ("two undecodable rows report the FIRST") covers
	// the undecodable arm and nothing else. A stream that MIXES shapes can, and it has
	// to hold in BOTH orders: each order exercises a different one of the two guards.
	//
	// This is diagnostic ordering, not a verdict — either error fails the run — but the
	// first drift is where the envelope actually moved and a later one is usually its
	// wake, so reporting the wrong one sends an operator to the wrong row.
	//
	// Kills: dropping `if unreadable == nil` on the roleless path (last-wins), and
	// dropping it on the undecodable path.
	t.Run("the FIRST unreadable row wins ACROSS shapes", func(t *testing.T) {
		malformed := ompMalformedAssistantMessageEndLine()
		roleless := ompDecodableFailedMessageEndWithoutRole(`"role":null,`)
		// PREMISE: the two fixtures really do take the two DIFFERENT arms. If either
		// drifted to the other arm this would silently become a same-shape stream,
		// which is exactly the stream that cannot distinguish the mutant.
		if json.Valid([]byte(strings.TrimSpace(malformed))) {
			t.Fatalf("malformed fixture is valid JSON, so both rows now take the same arm: %s", malformed)
		}
		var event ompStreamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(roleless)), &event); err != nil {
			t.Fatalf("roleless fixture does not decode (%v), so both rows now take the same arm: %s", err, roleless)
		}
		orders := []struct {
			name    string
			first   string
			second  string
			wants   string
			refuses string
		}{
			{name: "malformed first", first: malformed, second: roleless, wants: "MALFORMED", refuses: "no role"},
			{name: "roleless first", first: roleless, second: malformed, wants: "no role", refuses: "MALFORMED"},
		}
		for _, order := range orders {
			t.Run(order.name, func(t *testing.T) {
				stdout := ompHeaderLine(ompFixtureSessionID) +
					ompAssistantEnd("Let me read the file first.", 12, 3) +
					order.first + order.second + ompAgentEnd()
				text, _, _, err := parseOmpStreamJSON(stdout)
				if err == nil {
					t.Fatalf("parseOmpStreamJSON accepted a stream carrying two unreadable rows (text=%q)", text)
				}
				if !strings.Contains(err.Error(), order.wants) {
					t.Fatalf("error %q must report the FIRST unreadable row (the %s one)", err.Error(), order.name)
				}
				if strings.Contains(err.Error(), order.refuses) {
					t.Fatalf("error %q reports the SECOND unreadable row: the first is where the envelope moved", err.Error())
				}
			})
		}
	})

	// A message_end whose role is THERE and is not assistant stays skipped: this rule
	// closes a bypass, it does not widen rule 2's filter into "every message_end is
	// load-bearing". omp emits one for every tool result and every input message of a
	// turn, so failing those would fail every healthy run.
	//
	// Kills: failing on any message_end that is not an assistant message.
	t.Run("a message_end whose role is present and not assistant is still skipped", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompToolResultEnd() +
			ompAssistantEnd("done", 2, 1) +
			ompAbortedToolResultEnd("aborted", "Deadline exceeded") +
			ompAgentEnd()
		text, _, usage, err := parseOmpStreamJSON(stdout)
		if err != nil {
			t.Fatalf("parseOmpStreamJSON: %v (a message_end with a readable non-assistant role is a row rule 2 drops unread)", err)
		}
		if text != "done" {
			t.Fatalf("text = %q, want %q", text, "done")
		}
		if usage.InputTokens != 2 || usage.OutputTokens != 1 {
			t.Fatalf("usage = %d/%d, want 2/1: the tool-result rows' absurd counts must stay out of the bill", usage.InputTokens, usage.OutputTokens)
		}
	})
}

// TestOmpRawFieldIsPrecision pins the three precision guarantees ompRawFieldIs's doc
// claims — every occurrence of the key is scanned, the key must be followed by a
// colon, and the value must follow that colon immediately. None of the three had a
// detector before this test: the g7 verification at cb34f666 mutated each separately
// and all three survived the whole suite. That matters because ompRawFieldIs is the
// SOLE gate deciding whether a corrupt line fails a job — each loose reading is a
// silent move of the line between "skipped fragment" and "failed run".
//
// They survived because realistic omp fragments always put `"type"` and `"role"`
// first inside their own object, so no fixture built from real shapes separates the
// loose readings from the tight one. The fragments below are deliberately awkward
// bytes: that is this function's entire input domain — it runs only on lines
// encoding/json has already rejected, i.e. on cut, glued and interleaved text where
// no ordering is guaranteed.
//
// Each subtest asserts the helper's verdict AND the consequence on a whole stream,
// since a guarantee that does not change a run's outcome is not worth claiming.
func TestOmpRawFieldIsPrecision(t *testing.T) {
	// Kills: matching the value ANYWHERE after the colon (Contains) rather than at
	// its head.
	t.Run("the value must follow the colon immediately", func(t *testing.T) {
		// A row ABOUT a message_end is not a message_end. Its own `"type"` says
		// what it is, and the marker text appears later as another field's VALUE —
		// so a reading that accepts the value anywhere after the colon fails this
		// healthy run over a diagnostic line.
		fragment := `{"type":"notice","droppedEvent":"message_end","role":"assistant","detail":"the writer lost a chun`
		ompAssertFragmentIsRaw(t, fragment)
		if ompRawFieldIs(fragment, "type", "message_end") {
			t.Fatalf("ompRawFieldIs matched a value that is not this key's value: this row's type is notice, and message_end is some other field's content")
		}
		// The OTHER marker really is present, so the type marker is the only thing
		// keeping this line skipped — without that assertion the subtest could pass
		// for the wrong reason.
		if !ompRawFieldIs(fragment, "role", "assistant") {
			t.Fatalf("fixture no longer carries the role marker, so the type marker is no longer the only thing under test: %s", fragment)
		}
		if ompMalformedAssistantMessageEnd(fragment) {
			t.Fatalf("a notice row that merely NAMES message_end must not identify as one")
		}
		ompAssertFragmentSkipped(t, fragment)
	})

	// Kills: stopping at the first occurrence of the key that is not followed by a
	// colon (`return false` instead of `continue`).
	t.Run("every occurrence of the key is scanned", func(t *testing.T) {
		// The row's own `"role":"assistant"` is preceded by the WORD role appearing
		// as a tool argument's value. A scan that gives up at the first occurrence
		// never reaches the real field, the cut assistant message_end goes
		// unidentified, and the stale-sentence bypass reopens.
		fragment := `{"type":"message_end","message":{"content":[{"type":"toolCall","toolName":"edit",` +
			`"args":{"field":"role"}}],"role":"assistant","stopReason":"err`
		ompAssertFragmentIsRaw(t, fragment)
		if !ompRawFieldIs(fragment, "role", "assistant") {
			t.Fatalf("ompRawFieldIs stopped at an occurrence of the key that is not a field: a cut line may carry the key as a value or nested, and the real field comes later")
		}
		if !ompMalformedAssistantMessageEnd(fragment) {
			t.Fatalf("fragment carries both markers and must identify as a malformed assistant message_end")
		}
		ompAssertFragmentFailsRun(t, fragment)
	})

	// Kills: dropping the colon requirement (a key adjacent to the value matches).
	t.Run("the key must be followed by a colon", func(t *testing.T) {
		// A fragment whose colons did not survive is not a field-carrying row; it is
		// two strings next to each other, and identification here is drawn at
		// evidence, not resemblance (see ompMalformedAssistantMessageEnd's residual).
		// Pinning the skip is pinning that line: a reading that ignores the colon
		// turns any line that merely mentions both words in order into a failed job.
		fragment := `{"type" "message_end","message":{"role" "assistant","content":[{"type" "tex`
		ompAssertFragmentIsRaw(t, fragment)
		if ompRawFieldIs(fragment, "type", "message_end") {
			t.Fatalf("ompRawFieldIs matched a key that is not followed by a colon: adjacency is not a field")
		}
		if ompRawFieldIs(fragment, "role", "assistant") {
			t.Fatalf("ompRawFieldIs matched a key that is not followed by a colon: adjacency is not a field")
		}
		if ompMalformedAssistantMessageEnd(fragment) {
			t.Fatalf("a fragment with no surviving field structure must stay skipped")
		}
		ompAssertFragmentSkipped(t, fragment)
	})
}

// ompAssertFragmentIsRaw binds the premise every subtest above depends on: these
// fragments only ever reach ompRawFieldIs because the decoder rejected them outright.
// A fixture that became valid JSON would be classified from its fields instead, and
// the subtest would stop exercising the raw-text reading at all.
func ompAssertFragmentIsRaw(t *testing.T, fragment string) {
	t.Helper()
	if json.Valid([]byte(fragment)) {
		t.Fatalf("fixture is valid JSON, so it never reaches the raw-text reading under test: %s", fragment)
	}
}

// ompAssertFragmentSkipped runs a fragment through a stream that is otherwise healthy
// and demands the run still answers: the consequence of a false RED here is a
// completed job reported as envelope drift.
func ompAssertFragmentSkipped(t *testing.T, fragment string) {
	t.Helper()
	stdout := ompHeaderLine(ompFixtureSessionID) + fragment + "\n" +
		ompAssistantEnd("done", 2, 1) + ompAgentEnd()
	text, _, _, err := parseOmpStreamJSON(stdout)
	if err != nil {
		t.Fatalf("parseOmpStreamJSON: %v (this fragment identifies nothing, so rule 6 must skip it instead of failing a healthy run)", err)
	}
	if text != "done" {
		t.Fatalf("text = %q, want %q: the assistant DID answer", text, "done")
	}
}

// ompAssertFragmentFailsRun is the mirror: a fragment that DOES identify an assistant
// message_end must fail the run, and the earlier turn's sentence must not survive as
// the answer — the terminal agent_end is intact, so no other rule catches this.
func ompAssertFragmentFailsRun(t *testing.T, fragment string) {
	t.Helper()
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("Let me read the file first.", 12, 3) + fragment + "\n" + ompAgentEnd()
	text, _, _, err := parseOmpStreamJSON(stdout)
	if err == nil {
		t.Fatalf("parseOmpStreamJSON accepted a stream whose final assistant message_end was cut (text=%q)", text)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "malformed") {
		t.Fatalf("error %q must report the row as MALFORMED", err.Error())
	}
	if text != "" {
		t.Fatalf("text = %q, want none: that sentence was written BEFORE the row that was cut", text)
	}
}

// TestParseOmpStreamJSONRequiresTerminal is the loud-drift guard: no agent_end
// means the CLI died mid-stream or the envelope moved. Kills: treating that as an
// empty success, AND deleting this guard in favour of the isTerminal one — no
// agent_end structurally implies !lastTerminal, so the isTerminal guard would
// silently absorb this case and tell operators the run was "scheduling a
// continuation (retry, model fallback or auto-compaction)" when the CLI actually
// died mid-stream. The assertion below therefore demands the phrase only THIS
// guard emits, not the shared substring "agent_end".
func TestParseOmpStreamJSONRequiresTerminal(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd("done", 2, 1)
	text, sessionID, _, err := parseOmpStreamJSON(stdout)
	if err == nil {
		t.Fatalf("parseOmpStreamJSON accepted a truncated stream (text=%q)", text)
	}
	if !strings.Contains(err.Error(), "without an agent_end") {
		t.Fatalf("error %q must say the stream ended WITHOUT an agent_end event, distinctly from the non-terminal case", err.Error())
	}
	if !strings.Contains(err.Error(), "died mid-stream") {
		t.Fatalf("error %q must name the real cause (the CLI died mid-stream), not a scheduled continuation", err.Error())
	}
	if strings.Contains(err.Error(), "isTerminal") {
		t.Fatalf("error %q is the non-terminal guard's message: the two guards must stay distinguishable", err.Error())
	}
	if sessionID != ompFixtureSessionID {
		t.Fatalf("sessionID = %q, want the header id even on the failure path", sessionID)
	}
}

// TestOmpDeliverMalformedEnvelope: stdout that is not the expected envelope at all
// must fail with the evidence preserved. Kills: silent success on drift.
func TestOmpDeliverMalformedEnvelope(t *testing.T) {
	stdout := "omp: something went very wrong\nand it was not json\n"
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
	if err == nil {
		t.Fatalf("Deliver succeeded on a malformed envelope: %+v", result)
	}
	if result.Raw != stdout {
		t.Fatalf("Raw = %q, want the raw stdout as evidence", result.Raw)
	}
	if result.SessionDiag == nil {
		t.Fatal("SessionDiag = nil, want process evidence for a run that happened")
	}
	if result.SessionDiag.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty when no header was parsed", result.SessionDiag.SessionID)
	}
}

// TestOmpDeliverNonZeroExit: a real process failure must surface stderr AND keep
// the session diagnostics, including the header id the run did manage to print —
// AND the usage the partial stream already proved. A run that spent 1234/567 tokens
// and then died (an OOM kill, a crashed Bun binary, a SIGKILLed process group) spent
// them for real; 0/0 would hide the most expensive failures from a spend audit and
// contradict the parse-error path three lines below it, which books the same numbers
// for the same reason. Kills: dropping InputTokens/OutputTokens from the err != nil
// return.
func TestOmpDeliverNonZeroExit(t *testing.T) {
	stdout := ompHeaderLine(ompFixtureSessionID) + ompAssistantEnd("partial work before the crash", 1234, 567)
	runner := &fakeRunner{
		results: []subprocess.Result{{Stdout: stdout, Stderr: "omp: provider request failed"}},
		errs:    []error{errors.New("exit status 1")},
	}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
	if err == nil {
		t.Fatal("Deliver succeeded despite a non-zero exit")
	}
	if !strings.Contains(err.Error(), "omp: provider request failed") {
		t.Fatalf("error %q must carry the stderr detail", err.Error())
	}
	if result.Raw != stdout+"omp: provider request failed" {
		t.Fatalf("Raw = %q, want stdout+stderr on the failure path", result.Raw)
	}
	if result.InputTokens != 1234 || result.OutputTokens != 567 {
		t.Fatalf("usage = %d/%d, want 1234/567: the tokens the run burned before the process died were still billed",
			result.InputTokens, result.OutputTokens)
	}
	diag := result.SessionDiag
	if diag == nil {
		t.Fatal("SessionDiag = nil, want process evidence")
	}
	if !strings.Contains(diag.Stderr, "provider request failed") {
		t.Fatalf("SessionDiag.Stderr = %q, want the captured stderr", diag.Stderr)
	}
	if diag.SessionID != ompFixtureSessionID {
		t.Fatalf("SessionID = %q, want the header id parsed before the failure", diag.SessionID)
	}
	if !diag.StdoutSeen {
		t.Fatal("StdoutSeen = false, want true: the header line was printed")
	}
}

// TestOmpAuthFailureClassified kills a generic error message: omp routes to a
// provider per run, so an unclassified credential failure sends operators hunting
// through the wrong layer.
func TestOmpAuthFailureClassified(t *testing.T) {
	t.Run("non-zero exit with stderr", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "Error: 401 Unauthorized (no credentials configured)"}},
			errs:    []error{errors.New("exit status 1")},
		}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err == nil {
			t.Fatal("Deliver succeeded despite an auth failure")
		}
		if !strings.Contains(err.Error(), OmpAuthSetupMessage) {
			t.Fatalf("error %q must carry the omp auth setup guidance", err.Error())
		}
		if !strings.Contains(err.Error(), "/root/.local/bin") {
			t.Fatalf("error %q must name the install location", err.Error())
		}
	})

	t.Run("exit zero with a stream error", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompFailedAssistantEnd("error", "401 Unauthorized: invalid api key", 401) +
			ompAgentEnd()
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err == nil {
			t.Fatal("Deliver succeeded despite an auth failure reported on exit 0")
		}
		if !strings.Contains(err.Error(), OmpAuthSetupMessage) {
			t.Fatalf("error %q must carry the omp auth setup guidance", err.Error())
		}
	})

	t.Run("unrelated failure stays unclassified", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "Error: rate limit exceeded, retry after 60s"}},
			errs:    []error{errors.New("exit status 1")},
		}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err == nil {
			t.Fatal("Deliver succeeded despite a non-zero exit")
		}
		if strings.Contains(err.Error(), OmpAuthSetupMessage) {
			t.Fatalf("rate-limit failure %q was misclassified as an auth failure", err.Error())
		}
	})

	// ONE NEEDLE PER FIXTURE. The realistic fixtures above each match two needles
	// ("401 Unauthorized (no credentials configured)" matches unauthorized AND
	// credential), so dropping any single needle stays green. Each row here matches
	// exactly one, which is what makes every needle individually load-bearing —
	// omp routes to a provider per run, so the vocabulary its providers use for "I
	// have no usable credential" is the whole classifier.
	t.Run("every needle is individually load-bearing", func(t *testing.T) {
		cases := []struct {
			needle string
			stderr string
		}{
			{needle: "unauthorized", stderr: "Error: HTTP 401 Unauthorized"},
			{needle: "authentication", stderr: "provider rejected the request: authentication failed"},
			{needle: "authenticate", stderr: "run `omp auth login` to authenticate this profile"},
			{needle: "api key", stderr: "missing api key for provider openai"},
			{needle: "no model available", stderr: "no model available for role implement"},
			{needle: "credential", stderr: "the profile store holds no usable credential"},
		}
		for _, tc := range cases {
			t.Run(tc.needle, func(t *testing.T) {
				// Guard the guard: a fixture that matched a second needle would keep this
				// row green after its own needle was deleted.
				matched := 0
				for _, needle := range []string{"unauthorized", "authentication", "authenticate", "api key", "no model available", "credential"} {
					if strings.Contains(strings.ToLower(tc.stderr), needle) {
						matched++
					}
				}
				if matched != 1 {
					t.Fatalf("fixture %q matches %d needles, want exactly 1 (otherwise it cannot pin %q)", tc.stderr, matched, tc.needle)
				}
				runner := &fakeRunner{
					results: []subprocess.Result{{Stderr: tc.stderr}},
					errs:    []error{errors.New("exit status 1")},
				}
				adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
				_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
				if err == nil {
					t.Fatal("Deliver succeeded despite a non-zero exit")
				}
				if !strings.Contains(err.Error(), OmpAuthSetupMessage) {
					t.Fatalf("stderr %q was not classified as an auth failure: %v", tc.stderr, err)
				}
			})
		}
	})

	// The deliberate decision NOT to scan the whole stdout transcript. omp's stdout
	// carries the model's own prose, so a review that merely DISCUSSES api keys and
	// authentication would masquerade as a credential failure and send operators to
	// fix a credential that works. Kills: adding result.Stdout to the scanned parts.
	t.Run("a failing run whose transcript discusses api keys is not an auth failure", func(t *testing.T) {
		stdout := ompHeaderLine(ompFixtureSessionID) +
			ompAssistantEnd("The handler logs the raw api key and skips authentication entirely; flag both.", 5, 2) +
			ompFailedAssistantEnd("error", "rate limit exceeded, retry after 60s", 429) +
			ompAgentEnd()
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
		if err == nil {
			t.Fatal("Deliver succeeded on a run whose assistant turn failed")
		}
		if !strings.Contains(result.Raw, "api key") {
			t.Fatalf("fixture is toothless: Raw %q must carry the api-key prose the classifier must ignore", result.Raw)
		}
		if strings.Contains(err.Error(), OmpAuthSetupMessage) {
			t.Fatalf("a rate-limit failure was classified as an auth failure because the TRANSCRIPT mentions api keys: %v", err)
		}
	})
}

// TestOmpSummaryIsFinalAssistantTextNotEnvelope: the engine extracts
// gitmoot_result from what the adapter returns, so Summary/Raw must be the
// UNWRAPPED assistant text. Kills: Summary = stdout, where the fenced block would
// arrive JSON-escaped (\n instead of newlines) and every job would fail extraction.
func TestOmpSummaryIsFinalAssistantTextNotEnvelope(t *testing.T) {
	resultBlock := "```json\n{\"gitmoot_result\":{\"decision\":\"approve\",\"summary\":\"no blocking issues\"}}\n```"
	stdout := ompHeaderLine(ompFixtureSessionID) +
		ompAssistantEnd("thinking out loud", 3, 1) +
		ompAssistantEnd(resultBlock, 4, 2) +
		ompAgentEnd()
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	result, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "review"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.HasPrefix(result.Summary, "```json") {
		t.Fatalf("Summary = %q, want it to start with the fenced result block", result.Summary)
	}
	if strings.Contains(result.Summary, `\n`) {
		t.Fatalf("Summary %q carries escaped newlines: it is the NDJSON envelope, not the assistant text", result.Summary)
	}
	if strings.Contains(result.Summary, "message_end") {
		t.Fatalf("Summary %q leaked the stream envelope", result.Summary)
	}
	if result.Summary != resultBlock || result.Raw != resultBlock {
		t.Fatalf("Summary/Raw = %q/%q, want the final assistant text %q", result.Summary, result.Raw, resultBlock)
	}
}

// TestOmpPolicyArgs: every autonomy policy produces the SAME explicit
// --approval-mode=yolo. Kills: mapping read-only onto always-ask (under which
// every headless tool call throws, bricking the runtime) and dropping the flag
// (which inherits the host's tools.approvalMode). Read-only is enforced
// Gitmoot-side, not by omp's approval tier.
func TestOmpPolicyArgs(t *testing.T) {
	policies := []string{
		AutonomyPolicyAuto,
		AutonomyPolicyReadOnly,
		AutonomyPolicyWorkspaceWrite,
		AutonomyPolicyDangerFullAccess,
	}
	wantArgv := []string{"omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", "work"}
	for _, policy := range policies {
		t.Run(policy, func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			agent := ompTestAgent()
			agent.AutonomyPolicy = policy
			if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "work"}); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if !reflect.DeepEqual(runner.calls[0], wantArgv) {
				t.Fatalf("argv = %v, want %v", runner.calls[0], wantArgv)
			}
			for _, arg := range runner.calls[0] {
				if strings.HasPrefix(arg, "--approval-mode") && arg != "--approval-mode=yolo" {
					t.Fatalf("policy %q produced approval flag %q", policy, arg)
				}
			}
		})
	}
}

// TestOmpPromptQuotingHazards: the prompt must land as ONE token immediately after
// `--`, byte-identical, whatever it looks like. Kills the fleet's known scar
// class: a prompt reparsed as a flag (leading `-` exits 2), as an attachment
// (leading `@`), or split on `=`.
func TestOmpPromptQuotingHazards(t *testing.T) {
	prompts := []string{
		"@file.md summarize this attachment reference",
		"--force the review to fail",
		"-p",
		"a=b",
		"line one\nline two\nline three",
		`run "; rm -rf /" and report what happened`,
	}
	for i, prompt := range prompts {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
			adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
			if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt}); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			argv := runner.calls[0]
			if argv[len(argv)-1] != prompt {
				t.Fatalf("last argv token = %q, want the prompt verbatim %q", argv[len(argv)-1], prompt)
			}
			if argv[len(argv)-2] != "--" {
				t.Fatalf("token before the prompt = %q, want %q", argv[len(argv)-2], "--")
			}
			if ompCountToken(argv, "--") != 1 {
				t.Fatalf("argv has %d `--` separators, want exactly 1: %v", ompCountToken(argv, "--"), argv)
			}
		})
	}
}

// captureOmpRunner records each invocation's argv and — because the staged prompt
// directory is removed by a deferred cleanup — reads the staged file WHILE it
// still exists, i.e. during Run, before Deliver returns.
type captureOmpRunner struct {
	result     subprocess.Result
	lastArgs   []string
	attachArg  string
	stagedPath string
	stagedBody string
	stagedRead bool
}

func (r *captureOmpRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.lastArgs = append([]string{command}, args...)
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "@") {
			r.attachArg = arg
			r.stagedPath = strings.TrimPrefix(arg, "@")
			if body, err := os.ReadFile(r.stagedPath); err == nil {
				r.stagedBody = string(body)
				r.stagedRead = true
			}
		}
	}
	res := r.result
	res.Command = command
	res.Args = args
	return res, nil
}

func (r *captureOmpRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

// TestOmpSizeCeilingsArePinnedToLiterals pins both ceilings to the numbers their
// EXTERNAL sources fix them at. Every other ceiling test sizes its fixture from the
// symbol, so it only ever checks the guard against itself: the constants could
// drift to any value and stay green while the real limits did not move.
//
// What each drift costs:
//   - ompMaxArgvPromptBytes above ~128 KiB reinstates E2BIG at fork/exec
//     (MAX_ARG_STRLEN, #723) — invisible everywhere else because it happens in the
//     kernel, not in this package.
//   - ompMaxAttachmentBytes above 5 MiB reinstates the skipped-attachment defect:
//     omp's MAX_CLI_TEXT_BYTES (packages/coding-agent/src/cli/file-processor.ts:16)
//     makes it substitute `<file name="…">(skipped: too large …)</file>`
//     (file-processor.ts:49-55) and answer anyway, while ompAttachedPromptPointer
//     still asserts the full contents are included in the message.
func TestOmpSizeCeilingsArePinnedToLiterals(t *testing.T) {
	if ompMaxArgvPromptBytes != 100*1024 {
		t.Fatalf("ompMaxArgvPromptBytes = %d, want 100*1024: it must stay below Linux's 128 KiB MAX_ARG_STRLEN (#723)",
			ompMaxArgvPromptBytes)
	}
	if ompMaxAttachmentBytes != 5*1024*1024 {
		t.Fatalf("ompMaxAttachmentBytes = %d, want 5*1024*1024: omp's own MAX_CLI_TEXT_BYTES (file-processor.ts:16), above which it silently substitutes a `(skipped: too large)` placeholder",
			ompMaxAttachmentBytes)
	}
}

// TestOmpOversizePromptStaged: a prompt above the argv ceiling is delivered as an
// omp @attachment instead of an argv token. Kills E2BIG in production — a failure
// that is invisible in every other test because it happens in fork/exec.
func TestOmpOversizePromptStaged(t *testing.T) {
	t.Run("oversize prompt is attached", func(t *testing.T) {
		runner := &captureOmpRunner{result: subprocess.Result{Stdout: ompStreamOK}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		prompt := strings.Repeat("Z", 120*1024)
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if r := runner; r.stagedPath == "" {
			t.Fatalf("oversize prompt was not staged; argv=%v", r.lastArgs)
		}
		if !runner.stagedRead {
			t.Fatalf("staged file %s did not exist during Run", runner.stagedPath)
		}
		if runner.stagedBody != prompt {
			t.Fatalf("staged content mismatch: got len=%d want len=%d", len(runner.stagedBody), len(prompt))
		}
		if filepath.Base(runner.stagedPath) != "prompt.md" {
			t.Fatalf("staged file = %q, want a prompt.md", runner.stagedPath)
		}
		if !filepath.IsAbs(runner.stagedPath) {
			t.Fatalf("attachment %q must be an absolute path (omp resolves it itself)", runner.attachArg)
		}
		argv := runner.lastArgs
		// The attachment must sit immediately before `--`: after it, everything is
		// message text and the `@` would lose its meaning.
		if argv[len(argv)-3] != runner.attachArg || argv[len(argv)-2] != "--" {
			t.Fatalf("argv tail = %v, want [@<staged>/prompt.md -- <pointer>]", argv[len(argv)-3:])
		}
		pointer := argv[len(argv)-1]
		if len(pointer) >= ompMaxArgvPromptBytes {
			t.Fatalf("trailing prompt token is not argv-safe: len=%d", len(pointer))
		}
		if strings.HasPrefix(pointer, "-") || strings.HasPrefix(pointer, "@") {
			t.Fatalf("trailing prompt token %q would be reparsed by omp", pointer)
		}
		// Shape alone is not enough: an EMPTY pointer has no leading `-` or `@` and
		// is shorter than the ceiling, yet it is a turn with an attachment and NO
		// instruction. The pointer has to carry its load-bearing claim — that
		// prompt.md IS the complete task and is already inlined here.
		if pointer == "" {
			t.Fatal("trailing prompt token is empty: a run with an attachment and no message is a turn with no instruction")
		}
		for _, want := range []string{"prompt.md", "complete task"} {
			if !strings.Contains(pointer, want) {
				t.Fatalf("pointer %q must contain %q so the model knows the attachment is the task", pointer, want)
			}
		}
		if strings.Contains(strings.Join(argv, ""), prompt) {
			t.Fatal("the raw oversize prompt leaked into the argv despite attachment delivery")
		}
		if _, err := os.Stat(runner.stagedPath); !os.IsNotExist(err) {
			t.Fatalf("staged file %s was not cleaned up after Deliver (err=%v)", runner.stagedPath, err)
		}
		if _, err := os.Stat(filepath.Dir(runner.stagedPath)); !os.IsNotExist(err) {
			t.Fatalf("staged dir %s was not cleaned up after Deliver", filepath.Dir(runner.stagedPath))
		}
	})

	t.Run("just below the threshold is the plain path", func(t *testing.T) {
		runner := &captureOmpRunner{result: subprocess.Result{Stdout: ompStreamOK}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		prompt := strings.Repeat("y", ompMaxArgvPromptBytes-1)
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if runner.stagedPath != "" {
			t.Fatalf("sub-threshold prompt was staged to %s", runner.stagedPath)
		}
		want := []string{"omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", prompt}
		if !reflect.DeepEqual(runner.lastArgs, want) {
			t.Fatalf("sub-threshold argv diverged from the plain path (len=%d)", len(runner.lastArgs))
		}
	})

	// The threshold itself: the rule is "stage at >= ompMaxArgvPromptBytes", so a
	// prompt of exactly that size must be staged. Kills a `<=` boundary slip, which
	// would leave a 100 KiB prompt on the argv.
	t.Run("exactly at the threshold is staged", func(t *testing.T) {
		runner := &captureOmpRunner{result: subprocess.Result{Stdout: ompStreamOK}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		prompt := strings.Repeat("z", ompMaxArgvPromptBytes)
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if runner.stagedPath == "" {
			t.Fatalf("a prompt of exactly %d bytes was left on the argv", ompMaxArgvPromptBytes)
		}
		if runner.stagedBody != prompt {
			t.Fatalf("staged content mismatch: got len=%d want len=%d", len(runner.stagedBody), len(prompt))
		}
	})

	// Above omp's OWN attachment ceiling the file channel stops delivering the
	// prompt: omp warns on stderr and substitutes a `(skipped: too large)`
	// placeholder, then answers anyway. Kills: shipping a pointer message that
	// claims the contents are attached when they are not — the model would answer a
	// task it never received and the adapter would return that answer as a success.
	t.Run("above omp's attachment ceiling is refused loudly", func(t *testing.T) {
		runner := &captureOmpRunner{result: subprocess.Result{Stdout: ompStreamOK}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		prompt := strings.Repeat("q", ompMaxAttachmentBytes+1)
		_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt})
		if err == nil {
			t.Fatal("Deliver accepted a prompt above omp's @file attachment ceiling")
		}
		if !strings.Contains(err.Error(), "attachment ceiling") {
			t.Fatalf("error %q must name the attachment ceiling it tripped", err.Error())
		}
		if runner.lastArgs != nil {
			t.Fatalf("ran a subprocess despite an undeliverable prompt: %v", runner.lastArgs)
		}
	})

	t.Run("exactly at omp's attachment ceiling is still staged", func(t *testing.T) {
		runner := &captureOmpRunner{result: subprocess.Result{Stdout: ompStreamOK}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		prompt := strings.Repeat("q", ompMaxAttachmentBytes)
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: prompt}); err != nil {
			t.Fatalf("Deliver rejected a prompt omp would still inline whole: %v", err)
		}
		if runner.stagedPath == "" {
			t.Fatalf("a prompt of exactly the ceiling was not staged; argv=%v", runner.lastArgs)
		}
	})
}

// TestOmpDeliverMaxTimeFromDeadline: omp's own deadline flushes a complete NDJSON
// envelope, while gitmoot's context deadline SIGKILLs the process group and loses
// every byte. Kills: never emitting --max-time.
func TestOmpDeliverMaxTimeFromDeadline(t *testing.T) {
	t.Run("deadline present", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the test context lost its deadline")
		}
		// Bracket the emitted value with floor(0.9 × remaining) computed immediately
		// before and immediately after the call. The window is at most a second wide,
		// so it pins the 0.9 fraction AND floor-vs-ceil: a ceil (or a +1) lands above
		// upper, a different fraction lands outside entirely.
		upper := int(time.Until(deadline).Seconds() * 0.9)
		if _, err := adapter.Deliver(ctx, ompTestAgent(), Job{Prompt: "work"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		lower := int(time.Until(deadline).Seconds() * 0.9)
		value := ompFlagValue(runner.calls[0], "--max-time")
		if value == "" {
			t.Fatalf("argv carries no --max-time under a deadline: %v", runner.calls[0])
		}
		seconds, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("--max-time %q is not a plain second count: %v", value, err)
		}
		if seconds < lower || seconds > upper {
			t.Fatalf("--max-time = %d, want floor(0.9 × remaining) in [%d,%d]", seconds, lower, upper)
		}
	})

	t.Run("no deadline", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		if _, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if ompCountToken(runner.calls[0], "--max-time") != 0 {
			t.Fatalf("argv carries --max-time without a deadline: %v", runner.calls[0])
		}
	})
}

// ompNoBinaryRunner fails LookPath the way the daemon's PATH does when omp is not
// installed where the daemon can see it, and records any Run so the test can prove
// none happened.
type ompNoBinaryRunner struct {
	calls [][]string
}

func (r *ompNoBinaryRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	return subprocess.Result{}, nil
}

// LookPath fails ONLY for "omp" — every other name resolves. That pins the name
// the preflight actually looks up: a preflight probing some other binary would
// sail through here and be caught by this test's zero-subprocess assertions.
func (r *ompNoBinaryRunner) LookPath(file string) (string, error) {
	if file != "omp" {
		return "/usr/bin/" + file, nil
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
}

// TestOmpDeliverMissingBinary: a daemon-PATH problem must surface as a
// daemon-PATH problem. Kills: spawning anyway and reporting the resulting garbage.
func TestOmpDeliverMissingBinary(t *testing.T) {
	t.Run("Deliver", func(t *testing.T) {
		runner := &ompNoBinaryRunner{}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		_, err := adapter.Deliver(context.Background(), ompTestAgent(), Job{Prompt: "work"})
		if err == nil {
			t.Fatal("Deliver succeeded with no omp binary on PATH")
		}
		if !strings.Contains(err.Error(), "/root/.local/bin") {
			t.Fatalf("error %q must name the install location", err.Error())
		}
		if len(runner.calls) != 0 {
			t.Fatalf("ran %d subprocess(es) despite the failed preflight: %v", len(runner.calls), runner.calls)
		}
	})

	t.Run("Start", func(t *testing.T) {
		runner := &ompNoBinaryRunner{}
		adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
		agent := ompTestAgent()
		agent.RuntimeRef = ""
		_, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "hello"})
		if err == nil {
			t.Fatal("Start succeeded with no omp binary on PATH")
		}
		if !strings.Contains(err.Error(), "/root/.local/bin") {
			t.Fatalf("error %q must name the install location", err.Error())
		}
		if len(runner.calls) != 0 {
			t.Fatalf("ran %d subprocess(es) despite the failed preflight: %v", len(runner.calls), runner.calls)
		}
	})
}

// TestOmpStartReturnsUsableRef: registration needs the header id back, and the
// caller needs the assistant text — not the envelope. Kills: an unusable
// registration ref.
func TestOmpStartReturnsUsableRef(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	agent := ompTestAgent()
	agent.RuntimeRef = ""
	result, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: "introduce yourself"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.RuntimeRef != ompFixtureSessionID {
		t.Fatalf("RuntimeRef = %q, want the header session id %q", result.RuntimeRef, ompFixtureSessionID)
	}
	if err := adapter.Validate(context.Background(), Agent{
		Name: agent.Name, Role: agent.Role, Runtime: OmpRuntime, RuntimeRef: result.RuntimeRef, RepoScope: agent.RepoScope,
	}); err != nil {
		t.Fatalf("the ref Start returned does not validate: %v", err)
	}
	if result.Raw != "done" {
		t.Fatalf("Raw = %q, want the unwrapped assistant text", result.Raw)
	}
	runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", "introduce yourself")

	t.Run("empty prompt is rejected before any run", func(t *testing.T) {
		empty := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
		if _, err := (OmpAdapter{Runner: empty}).Start(context.Background(), StartRequest{Agent: agent, Prompt: "  "}); err == nil {
			t.Fatal("Start accepted an empty prompt")
		} else if len(empty.calls) != 0 {
			t.Fatalf("Start ran %d subprocess(es) for an empty prompt", len(empty.calls))
		}
	})

	// The session header is the ONLY channel that reports a finished print-mode
	// run's id, so a stream without one leaves the registration with no reference
	// at all. Kills: registering an omp agent whose RuntimeRef is silently empty,
	// AND handing back the parsed assistant text as Raw on this failure. The
	// missing header is an ENVELOPE defect, so the envelope is the evidence: the
	// raw stdout is the only thing that shows which line omp wrote first, and the
	// two failure returns above this one (non-zero exit, parse error) already
	// answer with raw stdout. A sentence lifted out of a stream whose id is gone
	// diagnoses nothing.
	t.Run("a stream with no session header is rejected", func(t *testing.T) {
		stdout := ompAssistantEnd("hi", 1, 1) + ompAgentEnd()
		headerless := &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}
		result, err := (OmpAdapter{Runner: headerless, Dir: "/repo"}).
			Start(context.Background(), StartRequest{Agent: agent, Prompt: "introduce yourself"})
		if err == nil {
			t.Fatalf("Start registered an agent from a stream with no session header (ref=%q)", result.RuntimeRef)
		}
		if !strings.Contains(err.Error(), "session header") {
			t.Fatalf("error %q must name the missing session header", err.Error())
		}
		if result.RuntimeRef != "" {
			t.Fatalf("RuntimeRef = %q, want none when the header was missing", result.RuntimeRef)
		}
		if result.Raw != stdout {
			t.Fatalf("Raw = %q, want the FULL stdout %q: the diagnostic envelope, not the parsed content", result.Raw, stdout)
		}
	})

	// Defense in depth: the CLI preflight (runtime.ValidateStartRequest) is not the
	// only guard. Kills: an adapter Start that skips the shared agent-field shape
	// checks every other runtime enforces, AND one that skips validateStart's
	// Validate() call — the ref grammar, the runtime match and the autonomy policy
	// live there, so without those rows a Start that never validated at all would
	// register an agent with an unusable RuntimeRef.
	t.Run("malformed agent fields are rejected before any run", func(t *testing.T) {
		cases := []struct {
			name  string
			mutdt func(*Agent)
		}{
			{name: "name with a slash", mutdt: func(a *Agent) { a.Name = "seat/reviewer" }},
			{name: "name with whitespace", mutdt: func(a *Agent) { a.Name = "seat reviewer" }},
			{name: "repo scope not owner/repo", mutdt: func(a *Agent) { a.RepoScope = "gitmoot" }},
			{name: "empty role", mutdt: func(a *Agent) { a.Role = "  " }},
			// The Validate()-owned checks: an unusable registration ref, another
			// runtime's agent, and a policy no argv mapping exists for.
			{name: "unusable runtime ref", mutdt: func(a *Agent) { a.RuntimeRef = "garbage" }},
			{name: "resume-grammar runtime ref", mutdt: func(a *Agent) { a.RuntimeRef = LastRef }},
			{name: "another runtime's agent", mutdt: func(a *Agent) { a.Runtime = KimiRuntime }},
			{name: "unknown autonomy policy", mutdt: func(a *Agent) { a.AutonomyPolicy = "manual" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				bad := agent
				tc.mutdt(&bad)
				runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
				if _, err := (OmpAdapter{Runner: runner, Dir: "/repo"}).
					Start(context.Background(), StartRequest{Agent: bad, Prompt: "introduce yourself"}); err == nil {
					t.Fatal("Start accepted a malformed agent")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("Start ran %d subprocess(es) for a malformed agent", len(runner.calls))
				}
			})
		}
	})
}

// TestOmpValidateRefShapes pins the ref grammar: omp mints its own session ids and
// never resumes, so the shapes it accepts are a UUID, fresh:<suffix>, or nothing —
// and "last" (another runtime's resume grammar) is rejected rather than silently
// treated as fresh.
func TestOmpValidateRefShapes(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "uuid", ref: ompFixtureSessionID},
		{name: "fresh", ref: "fresh:seat-reviewer"},
		{name: "fresh job", ref: FreshRefForJob("local-ask-a/review")},
		{name: "empty", ref: ""},
		{name: "garbage", ref: "garbage", wantErr: true},
		{name: "last", ref: LastRef, wantErr: true},
	}
	adapter := OmpAdapter{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := ompTestAgent()
			agent.RuntimeRef = tc.ref
			err := adapter.Validate(context.Background(), agent)
			if tc.wantErr && err == nil {
				t.Fatalf("Validate accepted ref %q", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate rejected ref %q: %v", tc.ref, err)
			}
		})
	}

	t.Run("runtime mismatch", func(t *testing.T) {
		agent := ompTestAgent()
		agent.Runtime = KimiRuntime
		if err := adapter.Validate(context.Background(), agent); err == nil {
			t.Fatal("Validate accepted an agent belonging to another runtime")
		}
	})

	// The DELIVER path is exactly as strict as the Start path. Every sibling
	// runtime's Validate reaches the shared field checks through validateRuntime ->
	// ValidateAgent -> validateAgentFields; omp hand-rolls Validate, so the checks
	// have to be called here explicitly or dispatch would accept an agent that
	// `agent start` refuses — the reverse of every other runtime and the reverse of
	// the defense-in-depth rationale. Kills: leaving the field checks on the Start
	// path only.
	t.Run("malformed agent fields are rejected on the deliver path too", func(t *testing.T) {
		cases := []struct {
			name  string
			mutdt func(*Agent)
		}{
			{name: "name with a slash", mutdt: func(a *Agent) { a.Name = "seat/reviewer" }},
			{name: "name with whitespace", mutdt: func(a *Agent) { a.Name = "seat reviewer" }},
			{name: "empty name", mutdt: func(a *Agent) { a.Name = "  " }},
			{name: "empty role", mutdt: func(a *Agent) { a.Role = "  " }},
			{name: "repo scope not owner/repo", mutdt: func(a *Agent) { a.RepoScope = "gitmoot" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				bad := ompTestAgent()
				tc.mutdt(&bad)
				if err := adapter.Validate(context.Background(), bad); err == nil {
					t.Fatal("Validate accepted a malformed agent the Start path rejects")
				}
				runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
				if _, err := (OmpAdapter{Runner: runner, Dir: "/repo"}).
					Deliver(context.Background(), bad, Job{Prompt: "work"}); err == nil {
					t.Fatal("Deliver accepted a malformed agent")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("Deliver ran %d subprocess(es) for a malformed agent: %v", len(runner.calls), runner.calls)
				}
			})
		}
	})

	// omp hand-rolls Validate instead of delegating to validateRuntime, so its own
	// NormalizeAutonomyPolicy call is the ONLY policy guard on this path. Kills:
	// dropping it, which would admit an unknown policy no argv mapping exists for.
	t.Run("unknown autonomy policy", func(t *testing.T) {
		agent := ompTestAgent()
		agent.AutonomyPolicy = "manual"
		if err := adapter.Validate(context.Background(), agent); err == nil {
			t.Fatal("Validate accepted the unknown autonomy policy \"manual\"")
		}
	})
}

// TestOmpCapabilities pins the advertised set. "produce" is absent on purpose:
// that path runs the runtime under Landlock, which is unprobed for omp's Bun
// binary, and the registry entry must stay byte-equal to this slice.
func TestOmpCapabilities(t *testing.T) {
	caps, err := (OmpAdapter{}).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	want := []string{"review", "implement", "ask"}
	if !reflect.DeepEqual(caps, want) {
		t.Fatalf("Capabilities() = %v, want %v", caps, want)
	}
}

// TestOmpHealthUsesLiveCheckPrompt: Health is a real delivery of the canned live
// check. Kills a Health that runs nothing (or a different prompt) and reports the
// seat as alive.
func TestOmpHealthUsesLiveCheckPrompt(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: ompStreamOK}}}
	adapter := OmpAdapter{Runner: runner, Dir: "/repo"}
	if err := adapter.Health(context.Background(), ompTestAgent()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	runner.want(t, 0, "omp", "-p", "--mode=json", "--approval-mode=yolo", "--no-session", "--", OmpLiveCheckPrompt)
}
