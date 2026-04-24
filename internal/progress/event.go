// Package progress is the wire format and server-side emitter for the
// NDJSON progress protocol spoken between the voodu server and the
// `voodu apply` client.
//
// Why a dedicated protocol:
//
//   - The legacy path emitted free-form `-----> Banner` lines on stdout.
//     The client used a regex-ish state machine (cmd/cli/stream_filter.go)
//     to detect step boundaries and swallow buildx chatter. Every new
//     server-side banner needed a matching table entry on the client;
//     every protocol upgrade risked stylistic drift between `-----> `,
//     `✓ `, and buildx `#N ...`.
//
//   - NDJSON — one JSON event per line — turns that implicit contract
//     into an explicit one. The server classifies lines as it emits them
//     (this is a step boundary, this is a log line, this is an apply
//     result), the client renders without guessing. Buildx chatter
//     becomes `{"type":"log","level":"info","text":"#3 DONE"}` — still
//     swallowable, but now on type, not regex.
//
// Channel decision (Channel A, decided in the Camada 3 design doc):
//
//	Events are written to the server's stdout. Stderr stays free for
//	panic traces / unexpected subprocess leakage. The client drains
//	stderr straight to the user's terminal without any filtering. If
//	the server disappears mid-stream, the last NDJSON frame the client
//	parsed is the last known state; whatever the server said on stderr
//	lands alongside verbatim, giving the user full context for the
//	failure. A single-channel design would have forced us to interleave
//	panic output with structured events — harder to parse, worse
//	diagnostics.
//
// Version negotiation:
//
//	The client injects `VOODU_PROTOCOL=ndjson/1` into the remote env.
//	Servers that understand it reply with a `hello` event as their
//	first line. Servers that don't (older binary, or a new version with
//	the feature off) emit the legacy text stream and the client falls
//	back to the legacy progressFilter. Negotiation is one-way: the
//	client always speaks the latest protocol, the server chooses. That
//	keeps the client deployable ahead of the server without breaking
//	anyone.
package progress

import "time"

// ProtocolVersion identifies the wire format. Bump when a breaking
// change to the event schema ships. Additive changes (new event type,
// new optional field) keep the same version — the client tolerates
// unknown types and ignores unknown fields.
const ProtocolVersion = "ndjson/1"

// EnvProtocol is the env-var name the client uses to tell the server
// "I speak NDJSON, please respond in kind." Inlined into the SSH
// command by remote.buildRemoteCommand via opts.Env — see
// cmd/cli/forward_remote.go's remoteEnv().
const EnvProtocol = "VOODU_PROTOCOL"

// EventType discriminates the six shapes a single NDJSON frame can
// take. Keep values short and lowercase — they go on the wire on every
// line and a streaming build can emit thousands.
type EventType string

const (
	// EventHello is the protocol handshake. Always the first line the
	// server emits under NDJSON mode. Carries Protocol (e.g. "ndjson/1")
	// so the client can confirm compatibility before processing further
	// frames.
	EventHello EventType = "hello"

	// EventStepStart opens a step — the thing that renders as a live
	// spinner on the client. Carries ID (stable identifier so end can
	// pair with start even if multiple steps were opened) and Label
	// (the human-readable headline).
	EventStepStart EventType = "step_start"

	// EventStepEnd closes a step. Client resolves the spinner to a
	// green ✓ (Status=ok) or a red ✗ (Status=fail). A canceled apply
	// flushes Status=cancel so the UI doesn't lie about success.
	EventStepEnd EventType = "step_end"

	// EventLog is a free-form message — buildx chatter, plugin
	// sub-banners, post-deploy hook output. Level is info/warn/error.
	// The client usually swallows info-level during an active step
	// (the spinner headline is the story) and surfaces warn/error
	// verbatim.
	EventLog EventType = "log"

	// EventResult is a per-manifest verdict from `voodu apply`:
	// "deployment/softphone/web applied", "service/clowk-net pruned".
	// Kind/Scope/Name identify the resource; Action is the verb
	// (applied, created, unchanged, deleted, pruned). Client renders
	// these as green ✓ lines after phase 3.
	EventResult EventType = "result"

	// EventSummary is the final wrap-up emitted at the end of a
	// pipeline — "Built web in 3s", "Deploy completed successfully".
	// One per logical operation. The client prints it on its own row
	// as the last thing before returning control.
	EventSummary EventType = "summary"
)

// StepStatus is the terminal state of a step. Strings not ints so
// `jq '.status'` stays readable when debugging from a `ssh -v` log.
type StepStatus string

const (
	// StatusOK is a step that ran to completion. Renders as green ✓.
	StatusOK StepStatus = "ok"

	// StatusFail is a step that failed. Error carries the message.
	// Renders as red ✗.
	StatusFail StepStatus = "fail"

	// StatusCancel is a step that was interrupted before completion —
	// SSH dropped, Ctrl-C, prompt canceled. Renders neutrally (the
	// spinner line is cleared, no ✓/✗ lie about the outcome).
	StatusCancel StepStatus = "cancel"
)

// LogLevel values for Event.Level when Type == EventLog.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is the single wire struct. One Event == one JSON object ==
// one line on stdout. Every field except Type and Ts is optional; the
// Type discriminator decides which subset is populated.
//
// We went with a flat struct rather than a tagged union (Type +
// Payload interface{}) because:
//
//   - The payload shapes overlap enough (ID/Label on both step events,
//     Kind/Scope/Name on result+log-error) that a union would need
//     six payload types and a custom UnmarshalJSON anyway.
//
//   - json.Encoder.Encode with omitempty gives us the compact
//     "only-populated-fields-on-wire" behavior for free.
//
//   - Downstream readers that don't care about type switching can
//     `jq '.label'` without digging through .payload.label.
type Event struct {
	// Type is the discriminator. Required on every frame.
	Type EventType `json:"type"`

	// Ts is the server's wall-clock when the event was emitted
	// (RFC3339Nano). Clients may use it to compute elapsed durations
	// independently, for replays, or for log correlation with the
	// controller's audit trail.
	Ts string `json:"ts,omitempty"`

	// Protocol — hello only. Version the server is speaking.
	Protocol string `json:"protocol,omitempty"`

	// ID — step_start / step_end pairing key. Short opaque string,
	// unique within one server invocation (the emitter's counter plus
	// a category prefix: "ship-1", "build-1"). Not a UUID — the
	// streams are too short for collisions to matter and short IDs
	// keep the wire compact.
	ID string `json:"id,omitempty"`

	// Label — step_start. Human-readable headline ("Building release",
	// "Shipping web (scope: softphone, context: .)"). Passed to the
	// client's spinner renderer verbatim; the server is responsible
	// for making it presentable.
	Label string `json:"label,omitempty"`

	// Status — step_end. See StepStatus constants.
	Status StepStatus `json:"status,omitempty"`

	// Error — step_end (when Status=fail) or log (when Level=error).
	// Short human-readable message; full stack traces belong in the
	// controller log, not on the wire.
	Error string `json:"error,omitempty"`

	// Level — log. info/warn/error per LogLevel constants.
	Level string `json:"level,omitempty"`

	// Text — log / summary. The message body.
	Text string `json:"text,omitempty"`

	// Kind — result. "deployment", "ingress", "service", etc.
	Kind string `json:"kind,omitempty"`

	// Scope — result. Empty for kinds that don't scope.
	Scope string `json:"scope,omitempty"`

	// Name — result. The resource's short name.
	Name string `json:"name,omitempty"`

	// Action — result. applied / created / unchanged / deleted / pruned.
	// Mirrors the verbs the legacy text path printed after the
	// resource reference.
	Action string `json:"action,omitempty"`
}

// Now returns the current time as RFC3339Nano, the format we stamp
// into Event.Ts. Wrapped so tests can substitute a fake clock without
// reaching through into time.Now directly.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
