package core

import (
	"encoding/json"
	"errors"
)

// ErrorCode is a stable, machine-readable error code. Codes are part of the
// API: never rename or reuse one.
type ErrorCode string

// Error codes.
const (
	CodeInvalidArgument ErrorCode = "invalid_argument"
	CodeNotFound        ErrorCode = "not_found"
	CodeConflict        ErrorCode = "conflict"
	CodeUnauthorized    ErrorCode = "unauthorized"
	CodeForbidden       ErrorCode = "forbidden"
	CodeUnavailable     ErrorCode = "unavailable"
	CodeVersionMismatch ErrorCode = "version_mismatch"
	CodeInternal        ErrorCode = "internal"
)

// ErrorCodes lists every error code.
var ErrorCodes = []ErrorCode{
	CodeInvalidArgument, CodeNotFound, CodeConflict, CodeUnauthorized,
	CodeForbidden, CodeUnavailable, CodeVersionMismatch, CodeInternal,
}

// Error is Gaia's typed error, encoded on the wire as
// {"code": …, "message": …, "details": {…}}. Details never hold secrets.
type Error struct {
	Code    ErrorCode
	Message string
	Details map[string]string
}

// NewError returns an Error with no details.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Error returns "code: message".
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is reports whether target is an *Error with the same code, so
// errors.Is(err, core.NewError(core.CodeNotFound, "")) matches any not_found.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) || t == nil {
		return false
	}
	return t.Code == e.Code
}

type errorWire struct {
	Code    ErrorCode         `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details"`
}

// MarshalJSON encodes the error with details always present.
func (e *Error) MarshalJSON() ([]byte, error) {
	d := e.Details
	if d == nil {
		d = map[string]string{}
	}
	return json.Marshal(errorWire{e.Code, e.Message, d})
}

// UnmarshalJSON decodes the wire shape.
func (e *Error) UnmarshalJSON(b []byte) error {
	var w errorWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*e = Error(w)
	return nil
}

// ErrorResponse is the body of every non-2xx API response.
type ErrorResponse struct {
	Error *Error `json:"error"`
}
