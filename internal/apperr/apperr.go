// Package apperr defines error kinds that the API maps to HTTP status codes.
package apperr

import (
	"errors"
	"fmt"
)

// Error kinds. Can be checked with errors.Is.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
	ErrConflict = errors.New("conflict")
)

// Error is an error with a kind and a user-readable message.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

// NotFound creates an error of kind ErrNotFound.
func NotFound(format string, args ...any) error {
	return &Error{Kind: ErrNotFound, Msg: fmt.Sprintf(format, args...)}
}

// Invalid creates an error of kind ErrInvalid.
func Invalid(format string, args ...any) error {
	return &Error{Kind: ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

// Conflict creates an error of kind ErrConflict.
func Conflict(format string, args ...any) error {
	return &Error{Kind: ErrConflict, Msg: fmt.Sprintf(format, args...)}
}
