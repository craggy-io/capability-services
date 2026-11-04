package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Code classifies an error so every projection can map it mechanically:
// HTTP status, MCP isError text, a Starlark error.
type Code string

const (
	Invalid     Code = "invalid"
	NotFound    Code = "not_found"
	Denied      Code = "denied"
	Conflict    Code = "conflict"
	Unavailable Code = "unavailable"
	Cancelled   Code = "cancelled"
	Internal    Code = "internal"
)

// Well-known reasons. A reason refines a Code with a stable,
// machine-readable cause so consumers can tell apart failures the closed
// Code set lumps together, without parsing message text. Reasons are
// lower_snake_case words (see ValidReason), never free text; the message
// carries the human detail. Adapters may use their own reasons in the same
// form; these are the ones shared across libraries.
const (
	// ReasonNoCapacity (unavailable): the backend is healthy but every
	// slot is taken, e.g. a full pool. The system is working as designed;
	// retrying later can succeed.
	ReasonNoCapacity = "no_capacity"
	// ReasonBackendDown (unavailable): the backend the capability depends
	// on has crashed, is unreachable or is otherwise not serving.
	ReasonBackendDown = "backend_down"
	// ReasonNotReady (unavailable): the registry refused the call because
	// the capability's readiness check is failing (set by Invoke/Start).
	ReasonNotReady = "not_ready"
	// ReasonClosed (unavailable): the registry has been closed and starts
	// no more jobs (set by Invoke/Start). Unlike the other unavailable
	// reasons, retrying against the same registry cannot succeed.
	ReasonClosed = "closed"
)

// ValidReason reports whether reason is a legal reason: lower_snake_case
// words of [a-z0-9] joined by single underscores, starting with a letter.
// The empty string means "no reason" and is also valid.
func ValidReason(reason string) bool {
	if reason == "" {
		return true
	}
	if reason[0] < 'a' || reason[0] > 'z' || reason[len(reason)-1] == '_' {
		return false
	}
	prev := byte(0)
	for i := 0; i < len(reason); i++ {
		c := reason[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_' && prev != '_':
		default:
			return false
		}
		prev = c
	}
	return true
}

func mustReason(reason string) {
	if !ValidReason(reason) {
		panic(fmt.Sprintf("registry: invalid error reason %q: lower_snake_case words only", reason))
	}
}

// Error is an error carrying a Code and, optionally, a Reason within it.
type Error struct {
	Code    Code
	Reason  string // optional stable refinement of Code, e.g. ReasonNoCapacity
	Message string
	Err     error // wrapped cause, if any
}

func (e *Error) Error() string {
	if e.Message != "" {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether a call that failed with c may succeed if made
// again unchanged, later: only Unavailable (a full pool, a backend that is
// down or not ready yet). Every other code needs a different call — fixed
// input, another target, re-read state — or none.
func (c Code) Retryable() bool { return c == Unavailable }

// ErrorInfo is the JSON form of a coded error, carried in JobStatus and in
// the ErrorEnvelope every projection returns.
type ErrorInfo struct {
	Code      Code   `json:"code"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"` // Code.Retryable, spelled out for the model
}

// ErrorEnvelope is a failed call's body in every projection: the HTTP
// error response, and the text an MCP or in-process tool result carries
// for the model, e.g.
// {"error":{"code":"unavailable","reason":"no_capacity","message":"…","retryable":true}}.
type ErrorEnvelope struct {
	Error *ErrorInfo `json:"error"`
}

// Info converts err to its ErrorInfo form, or nil if err is nil. Code,
// reason and message come from the outermost *Error in err's chain.
func Info(err error) *ErrorInfo {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return &ErrorInfo{Code: e.Code, Reason: e.Reason, Message: errorMessage(e), Retryable: e.Code.Retryable()}
	}
	code := CodeOf(err)
	return &ErrorInfo{Code: code, Message: err.Error(), Retryable: code.Retryable()}
}

func errorMessage(e *Error) string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Code)
}

// Errorf builds a coded error.
func Errorf(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ReasonErrorf builds a coded error with a reason. It panics if reason is
// not ValidReason: reasons are constants, so a bad one is a programming
// error.
func ReasonErrorf(code Code, reason, format string, args ...any) error {
	mustReason(reason)
	return &Error{Code: code, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// WrapReason annotates err with code and reason, keeping it unwrappable. A
// nil err passes through. Unlike WrapCode it always applies: the caller is
// classifying err deliberately, so code and reason replace any the err
// already carries (its message is kept). It panics if reason is not
// ValidReason.
func WrapReason(code Code, reason string, err error) error {
	mustReason(reason)
	if err == nil {
		return nil
	}
	return &Error{Code: code, Reason: reason, Message: Info(err).Message, Err: err}
}

// WrapCode annotates err with a code, keeping it unwrappable. A nil err and
// an err that already carries a code pass through unchanged.
func WrapCode(code Code, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return &Error{Code: code, Message: err.Error(), Err: err}
}

// CodeOf extracts the code from err. context cancellation and deadlines map
// to Cancelled; anything uncoded maps to Internal.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Cancelled
	}
	return Internal
}

// ReasonOf extracts the reason from err: that of the outermost *Error in
// its chain (the same one CodeOf reads), or "" when there is none.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Codes is a per-adapter sentinel → Code table. Each domain adapter declares
// one and translates its own errors in exactly one place. Sentinels in one
// table must not overlap: no error should match two sentinels mapped to
// different codes (see Wrap).
type Codes map[error]Code

// Wrap returns err annotated with the code of the sentinel it matches (by
// errors.Is). Nil, already-coded, and unmatched errors pass through. An
// error matching several sentinels gets their code when they agree; when
// they disagree the table is ambiguous, and Wrap returns an internal error
// naming the clashing codes instead of picking one, so the result never
// depends on map iteration order.
func (c Codes) Wrap(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	var matched []Code
	for sentinel, code := range c {
		if errors.Is(err, sentinel) && !slices.Contains(matched, code) {
			matched = append(matched, code)
		}
	}
	switch len(matched) {
	case 0:
		return err
	case 1:
		return &Error{Code: matched[0], Message: err.Error(), Err: err}
	}
	names := make([]string, len(matched))
	for i, code := range matched {
		names[i] = string(code)
	}
	sort.Strings(names)
	return &Error{Code: Internal, Err: err,
		Message: fmt.Sprintf("registry.Codes: error matches sentinels with different codes (%s): %v", strings.Join(names, ", "), err)}
}
