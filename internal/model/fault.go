package model

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
)

// FaultKind classifies a recoverable model-call failure — the fault classes the
// agent loop can absorb (compact-and-retry, re-prompt, escalate, or end the turn
// gracefully) instead of aborting the whole run. FaultNone means the error is not
// a recognised recoverable fault and the caller should treat it as fatal.
type FaultKind string

const (
	// FaultNone is "not a recoverable model-call fault".
	FaultNone FaultKind = ""
	// FaultContextOverflow is the provider rejecting a prompt that exceeds the
	// model's context window (llama.cpp HTTP 400 exceed_context_size_error, the
	// Anthropic "prompt is too long" 400). Recovery: compact the transcript and
	// retry, or escalate to a larger-window rung.
	FaultContextOverflow FaultKind = "context_overflow"
	// FaultMalformedToolCall is the model emitting a truncated/invalid tool-call
	// arguments blob the adapter cannot decode. Recovery: a bounded corrective
	// re-prompt, then escalate.
	FaultMalformedToolCall FaultKind = "malformed_tool_call"
	// FaultTimeout is a model call exceeding its deadline (the per-call HTTP
	// timeout, or the per-turn context deadline). Recovery: shrink the prompt and
	// retry / escalate while turn budget remains; end the turn gracefully once the
	// turn deadline itself is spent.
	FaultTimeout FaultKind = "timeout"
	// FaultRateLimit is the provider throttling the caller (HTTP 429
	// rate_limit_error, or 529 overloaded). It is transient and recoverable:
	// back off and retry the same rung, then DE-escalate to the free local floor
	// for the turn rather than retrying a throttled paid rung. Unlike the other
	// faults it never escalates UP — climbing into a more-rate-limited rung is the
	// wrong move (bug model-call-rate-limit-429-not-recoverable-aborts-run).
	FaultRateLimit FaultKind = "rate_limit"
)

// Sentinel errors adapters wrap so the loop classifies a fault structurally
// rather than by string-matching across layers. ClassifyFault unwraps to these.
var (
	// ErrContextOverflow marks an adapter error as a context-window overflow.
	ErrContextOverflow = errors.New("model context window exceeded")
	// ErrMalformedToolCall marks an adapter error as an undecodable tool call.
	ErrMalformedToolCall = errors.New("model emitted a malformed tool call")
	// ErrRateLimit marks an adapter error as a provider rate-limit / overloaded
	// rejection (429/529) — transient and recoverable via backoff + de-escalation.
	ErrRateLimit = errors.New("model provider rate-limited the request")
)

// ClassifyFault maps an adapter error onto the recoverable-fault taxonomy. It
// recognises the wrapped sentinels (ErrContextOverflow, ErrMalformedToolCall)
// and any deadline/timeout error (the per-call HTTP timeout or the per-turn
// context deadline). Anything else is FaultNone — fatal to the caller.
func ClassifyFault(err error) FaultKind {
	switch {
	case err == nil:
		return FaultNone
	case errors.Is(err, ErrContextOverflow):
		return FaultContextOverflow
	case errors.Is(err, ErrMalformedToolCall):
		return FaultMalformedToolCall
	case errors.Is(err, ErrRateLimit):
		return FaultRateLimit
	case isTimeout(err):
		return FaultTimeout
	default:
		return FaultNone
	}
}

// isTimeout reports whether err is a deadline/timeout: the request context's
// deadline, or a transport-level net.Error that timed out (http.Client.Timeout).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// overflowMarkers are substrings that, in a 4xx error body, indicate the prompt
// exceeded the model's context window. They span llama.cpp (exceed_context_size)
// and the OpenAI/Anthropic-style "context length/window" and "prompt is too long"
// phrasings, so a single adapter check covers the OpenAI-compatible providers.
var overflowMarkers = []string{
	"exceed_context_size",
	"context size",
	"context length",
	"context window",
	"maximum context",
	"too many tokens",
	"prompt is too long",
	"reduce the length",
	"n_ctx",
}

// LooksLikeContextOverflow reports whether an HTTP error response (its status and
// body) indicates a context-window overflow. Adapters call it to wrap such a
// rejection as ErrContextOverflow so the loop can compact-and-retry instead of
// aborting. Only the rejection statuses an overflow uses (400 Bad Request, 413
// Payload Too Large) are considered, to avoid misclassifying unrelated errors.
func LooksLikeContextOverflow(status int, body string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusInternalServerError:
		// Candidate statuses: llama.cpp reports an overflow as a 400
		// (exceed_context_size_error) OR a 500 ("Context size has been exceeded.",
		// type server_error); other OpenAI-compatible servers use 413. The body
		// marker below is the real discriminator, so a generic 500 (no marker) stays
		// fatal — only an overflow-marked one is treated as recoverable.
	default:
		return false
	}
	b := strings.ToLower(body)
	for _, marker := range overflowMarkers {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}

// statusOverloaded is Anthropic's non-standard "overloaded" code (not in
// net/http). Treated identically to 429: transient, back-off-able throttling.
const statusOverloaded = 529

// LooksLikeRateLimit reports whether an HTTP status is a transient throttling
// rejection — 429 Too Many Requests (the OpenAI/Anthropic rate_limit_error) or
// 529 overloaded. Adapters wrap such a status as ErrRateLimit so the loop backs
// off and de-escalates instead of aborting. Status alone is the discriminator
// (unlike overflow, no body marker is needed — 429/529 are unambiguous).
func LooksLikeRateLimit(status int) bool {
	return status == http.StatusTooManyRequests || status == statusOverloaded
}
