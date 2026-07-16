// Package tool defines the provider-agnostic tool-dispatch contract for the
// Corp-OS agent loop. The loop dispatches every tool call through a Provider,
// so the backing implementation is swappable without touching the loop:
// toolkit-server over MCP today (see package mcp), and native in-process Go
// organs or external MCP servers later. This seam is the forward-compatibility
// commitment recorded in the design orientation doc.
package tool

import "context"

// ErrorClass categorizes a failed dispatch; the empty class means success. The
// classes double as the escalation signal source (see Tally) — a provider that
// keeps failing a tool trips the repeated-tool-error trigger once the router lands.
type ErrorClass string

const (
	// ClassNone is the success class (no error).
	ClassNone ErrorClass = ""
	// ClassTransient is a network/timeout failure reaching the provider.
	ClassTransient ErrorClass = "transient"
	// ClassTool is a structured error the provider returned (an {"error": …} body).
	ClassTool ErrorClass = "tool_error"
	// ClassUsage is a WORKER-RECOVERABLE malformed/mistargeted call: bad or missing
	// params, a precondition not met (read-before-edit), a no-op edit, a wrong path.
	// The worker fixes it itself on the next round from the fed-back error, so it is
	// a failure (OK=false, folded into the transcript) but is NOT counted toward the
	// repeated_tool_error escalation trigger — climbing to a costlier MODEL does not
	// fix a deterministic usage error, and a floor that thrashes usage errors without
	// converging is still caught by the no-progress breaker (which IS escalatable).
	// This keeps a recoverable param slip from prematurely promoting a turn to the
	// strong rung (bug escalate-after-1-sends-recoverable-param-error-turns-to-opus-…).
	ClassUsage ErrorClass = "usage_error"
	// ClassParse is an undecodable response body.
	ClassParse ErrorClass = "parse_failure"
	// ClassFatal is an unexpected, unhandleable response.
	ClassFatal ErrorClass = "fatal"
)

// Call is a model's request to invoke one tool.
type Call struct {
	// ID is the provider-assigned call id, echoed back on the result message.
	ID string
	// Surface is the tool name / toolkit-server surface (work, knowledge, fs, …).
	Surface string
	// Action is the action to dispatch on that surface.
	Action string
	// Params are the action parameters.
	Params map[string]any
	// Rationale is optional; the substrate requires one for write actions.
	Rationale string
}

// Spec advertises a tool to the model: a name, a one-line capability summary,
// and a JSON Schema for its arguments. A Provider produces the specs it offers;
// the model emits Calls against them.
type Spec struct {
	// Name is the tool name presented to the model (a toolkit-server surface).
	Name string
	// Description is a one-line capability summary.
	Description string
	// InputSchema is the JSON Schema for the tool arguments (the action+params envelope).
	InputSchema map[string]any
}

// Result is the outcome of one dispatch. It never carries a Go error: a failure
// is reported as a non-empty ErrorClass with detail in Value, so the loop folds
// a failed call back into the transcript instead of aborting the turn.
type Result struct {
	// Call is the originating request.
	Call Call
	// OK reports whether the dispatch succeeded.
	OK bool
	// Value is the decoded result, or the provider's {"error": …} body on failure.
	Value any
	// ErrorClass categorizes a failure; ClassNone on success.
	ErrorClass ErrorClass
	// LatencyMS is the wall-clock dispatch latency in milliseconds.
	LatencyMS int64
	// SpanID is the substrate span id when the response surfaces one.
	SpanID string
}

// Fail builds a failed Result whose Value carries the error message under the "error" key —
// the one shape every dispatching provider returns on failure, so the loop folds a toolkit-
// client, web, or aggregate failure into the transcript identically. It is the single
// definition the per-provider fail builders collapsed into.
func Fail(call Call, class ErrorClass, msg string, latency int64) Result {
	return Result{
		Call:       call,
		OK:         false,
		Value:      map[string]any{"error": msg},
		ErrorClass: class,
		LatencyMS:  latency,
	}
}

// Provider dispatches a tool call and classifies the outcome. Implementations
// must not return errors out of band — every failure is reported through
// Result.ErrorClass so the loop never aborts a turn on a tool failure.
type Provider interface {
	Dispatch(ctx context.Context, c Call) Result
}

// ErrorTally counts error classes across a turn's dispatches. It is the source
// of the escalation triggers (repeated_tool_error, parse_failure) once the
// router lands.
type ErrorTally struct {
	// ToolErrors counts failed calls that signal a possible MODEL-capability
	// problem (tool_error/fatal/parse/transient) — the repeated_tool_error
	// escalation source. It deliberately EXCLUDES ClassUsage: a worker-recoverable
	// usage slip is not a reason to climb the model ladder.
	ToolErrors int
	// UsageErrors counts worker-recoverable usage failures (ClassUsage). Tracked
	// for observability; NOT folded into ToolErrors, so it never trips escalation.
	UsageErrors int
	// ParseFailures counts undecodable responses.
	ParseFailures int
	// TransientErrors counts network/timeout failures.
	TransientErrors int
}

// Tally counts error classes across results. A parse failure and a transient
// failure each also count as a tool error (the turn observed a failed call), so
// a flapping provider still trips the repeated-tool-error trigger. A ClassUsage
// failure is counted separately (UsageErrors) and NOT as a tool error — the
// worker self-corrects it, so it must not promote the turn to a costlier rung.
func Tally(results []Result) ErrorTally {
	var t ErrorTally
	for _, r := range results {
		switch r.ErrorClass {
		case ClassParse:
			t.ParseFailures++
			t.ToolErrors++
		case ClassTransient:
			t.TransientErrors++
			t.ToolErrors++
		case ClassTool, ClassFatal:
			t.ToolErrors++
		case ClassUsage:
			t.UsageErrors++
		case ClassNone:
			// success — not tallied
		}
	}
	return t
}
