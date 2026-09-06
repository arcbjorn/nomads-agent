package nomads

import (
	"errors"
	"fmt"
	"time"
)

// ErrorCode is a stable, agent-facing classification of a failure. Agents and
// the MCP layer branch on these instead of parsing HTTP details.
type ErrorCode string

const (
	// ErrAuthExpired means the stored session is no longer valid.
	ErrAuthExpired ErrorCode = "AUTH_EXPIRED"
	// ErrProfileValidationFailed means Nomads.com rejected a field value.
	ErrProfileValidationFailed ErrorCode = "PROFILE_VALIDATION_FAILED"
	// ErrTripAlreadyExists means an identical trip is already present.
	ErrTripAlreadyExists ErrorCode = "TRIP_ALREADY_EXISTS"
	// ErrTripAmbiguous means a desired trip matched several remote trips.
	ErrTripAmbiguous ErrorCode = "TRIP_AMBIGUOUS"
	// ErrTripNotFound means the referenced trip id is unknown.
	ErrTripNotFound ErrorCode = "TRIP_NOT_FOUND"
	// ErrRemoteStateMismatch means a write reported success but verification
	// found different state.
	ErrRemoteStateMismatch ErrorCode = "REMOTE_STATE_MISMATCH"
	// ErrRateLimited means Nomads.com asked us to slow down.
	ErrRateLimited ErrorCode = "RATE_LIMITED"
	// ErrAPIChanged means the response did not match the observed protocol.
	ErrAPIChanged ErrorCode = "NOMADS_API_CHANGED"
	// ErrGeocodeFailed means a city could not be resolved to coordinates.
	ErrGeocodeFailed ErrorCode = "GEOCODE_FAILED"
	// ErrInvalidInput means the caller's arguments were malformed.
	ErrInvalidInput ErrorCode = "INVALID_INPUT"
	// ErrNetwork means the request never produced a usable response.
	ErrNetwork ErrorCode = "NETWORK_ERROR"
)

// Error is a semantic failure carrying non-secret diagnostic context.
type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// RetryAfter is set for ErrRateLimited.
	RetryAfter time.Duration `json:"-"`
	wrapped    error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// Is supports errors.Is against a bare ErrorCode sentinel.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// WithDetail attaches a diagnostic field. Never pass credentials.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// Errorf builds a semantic error.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds a semantic error that preserves an underlying cause.
func Wrap(err error, code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), wrapped: err}
}

// CodeOf extracts the semantic code from an error, or "" if it has none.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsCode reports whether err carries the given semantic code.
func IsCode(err error, code ErrorCode) bool { return CodeOf(err) == code }
