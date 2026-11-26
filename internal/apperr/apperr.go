// Package apperr definiert Fehlerarten, die die API auf HTTP-Statuscodes abbildet.
package apperr

import (
	"errors"
	"fmt"
)

// Fehlerarten. Mit errors.Is prüfbar.
var (
	ErrNotFound = errors.New("nicht gefunden")
	ErrInvalid  = errors.New("ungültige Eingabe")
	ErrConflict = errors.New("Konflikt")
)

// Error ist ein Fehler mit Art und nutzerlesbarer Meldung.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

// NotFound erzeugt einen Fehler der Art ErrNotFound.
func NotFound(format string, args ...any) error {
	return &Error{Kind: ErrNotFound, Msg: fmt.Sprintf(format, args...)}
}

// Invalid erzeugt einen Fehler der Art ErrInvalid.
func Invalid(format string, args ...any) error {
	return &Error{Kind: ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

// Conflict erzeugt einen Fehler der Art ErrConflict.
func Conflict(format string, args ...any) error {
	return &Error{Kind: ErrConflict, Msg: fmt.Sprintf(format, args...)}
}
