// Package apperr defines the stable error vocabulary shared by the domain,
// service, repository and HTTP layers of the trail permit dispatch platform.
package apperr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine readable error identifier. Codes are part of the
// public API contract and must never be renamed without a version bump.
type Code string

const (
	// CodeInvalidArgument marks malformed or semantically invalid input.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeUnauthenticated marks a missing, expired or revoked credential.
	CodeUnauthenticated Code = "unauthenticated"
	// CodePermissionDenied marks an authenticated actor without the required role.
	CodePermissionDenied Code = "permission_denied"
	// CodeNotFound marks a missing addressable resource.
	CodeNotFound Code = "not_found"
	// CodeConflict marks a uniqueness or duplicate-request conflict.
	CodeConflict Code = "conflict"
	// CodeStateInvalid marks an illegal state machine transition.
	CodeStateInvalid Code = "state_invalid"
	// CodeVersionConflict marks a lost optimistic-lock race.
	CodeVersionConflict Code = "version_conflict"
	// CodeQuotaExhausted marks a permit window without remaining seats.
	CodeQuotaExhausted Code = "quota_exhausted"
	// CodeDeadlineExceeded marks a cancelled or timed out context.
	CodeDeadlineExceeded Code = "deadline_exceeded"
	// CodeUnavailable marks a dependency that is temporarily not usable.
	CodeUnavailable Code = "unavailable"
	// CodeInternal marks an unexpected failure.
	CodeInternal Code = "internal"
)

// Error carries a stable code, an operator readable message, an optional input
// field reference and the wrapped cause. The cause is preserved so callers can
// keep using errors.Is and errors.As across layers.
type Error struct {
	Code    Code
	Message string
	Field   string
	cause   error
}

// Error implements the error interface and keeps the cause visible in logs.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// WithField returns a copy that points at a specific request field.
func (e *Error) WithField(field string) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Field = field
	return &clone
}

// New builds an error without a cause.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an error without a cause using printf formatting.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a stable code and message to an existing cause.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// Wrapf attaches a stable code and formatted message to an existing cause.
func Wrapf(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

// CodeOf reports the closest stable code carried by err. Context cancellation is
// translated so callers do not have to special case it at every layer.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CodeDeadlineExceeded
	}
	return CodeInternal
}

// Is reports whether err carries the given stable code.
func Is(err error, code Code) bool {
	return CodeOf(err) == code
}

// FieldOf returns the offending request field when the error carries one.
func FieldOf(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Field
	}
	return ""
}

// Message returns a operator readable message for err, falling back to a
// generic sentence so internal details never leak to API clients.
func Message(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Message
	}
	switch CodeOf(err) {
	case CodeDeadlineExceeded:
		return "请求已取消或超时"
	default:
		return "服务内部错误"
	}
}

// HTTPStatus maps a stable code onto an HTTP status code.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeStateInvalid, CodeVersionConflict, CodeQuotaExhausted:
		return http.StatusConflict
	case CodeDeadlineExceeded:
		return http.StatusRequestTimeout
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Retryable reports whether a failed operation may succeed when retried with
// the same input. Worker backoff and HTTP clients share this decision.
func Retryable(err error) bool {
	switch CodeOf(err) {
	case CodeVersionConflict, CodeUnavailable, CodeDeadlineExceeded:
		return true
	default:
		return false
	}
}
