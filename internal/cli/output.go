package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/api"
)

// EnvelopeVersion is the "v" field of every JSON response. Bump it only for
// breaking changes; agents depend on this shape.
const EnvelopeVersion = 1

// Exit codes. Agents branch on these, so they are part of the contract.
const (
	ExitOK         = 0
	ExitError      = 1
	ExitUsage      = 2
	ExitBlocked    = 3 // blocked by policy
	ExitNeedsHuman = 4 // policy routed the change to a person
)

// Error codes used in the JSON envelope.
const (
	CodeError          = "error"
	CodeUsage          = "usage"
	CodeNotImplemented = "not_implemented"
	CodeCanceled       = "canceled"
)

// Envelope is the only shape --json ever prints on stdout:
//
//	{"v":1,"ok":true,"data":{...}}
//	{"v":1,"ok":false,"error":{"code":"...","message":"...","details":{...}}}
//
// details is optional: a few errors (sync_conflict) carry structured data an
// agent needs to recover.
type Envelope struct {
	V     int        `json:"v"`
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
}

// ErrorBody is the error half of the envelope.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Error is a command failure with a stable code and exit status.
type Error struct {
	Code    string
	Message string
	Exit    int
	Details any // optional, printed as error.details in JSON mode
}

func (e *Error) Error() string { return e.Message }

// UsageError reports bad arguments or flags (exit 2).
func UsageError(format string, args ...any) error {
	return &Error{Code: CodeUsage, Message: fmt.Sprintf(format, args...), Exit: ExitUsage}
}

// NotImplemented reports a command that is declared but not built yet.
func NotImplemented(command string) error {
	return &Error{
		Code:    CodeNotImplemented,
		Message: fmt.Sprintf("wayglass %s is not implemented yet", command),
		Exit:    ExitError,
	}
}

// exitStatus ends a command that already printed its result with a non-zero
// exit code, e.g. a successful propose whose decision is block (3).
type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// WithExit returns an error that sets the exit code without printing anything.
func WithExit(code int) error { return exitStatus{code: code} }

// Output writes command results. Commands never write to stdout directly.
type Output struct {
	JSON bool
	Out  io.Writer
	Err  io.Writer
}

// Emit prints a successful result: the envelope in JSON mode, otherwise the
// human rendering.
func (o Output) Emit(data any, human func(w io.Writer) error) error {
	if o.JSON {
		return writeEnvelope(o.Out, Envelope{V: EnvelopeVersion, OK: true, Data: data})
	}
	return human(o.Out)
}

// Warn prints a warning for people. In JSON mode warnings travel inside
// the data instead, so stderr stays quiet for agents.
func (o Output) Warn(format string, args ...any) {
	if o.JSON {
		return
	}
	_, _ = lipgloss.Fprintln(o.Err, warnLabel.Render("warning:")+" "+fmt.Sprintf(format, args...))
}

// Interactive reports whether prompts, colour and spinners are allowed:
// never with --json, and only when both stdin and stdout are terminals.
func (o Output) Interactive() bool {
	return !o.JSON && isTerminal(o.Out) && isTerminal(os.Stdin)
}

func writeEnvelope(w io.Writer, e Envelope) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(e)
}

func isTerminal(v any) bool {
	f, ok := osFile(v)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

var (
	errorLabel = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Red)
	warnLabel  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Yellow)
)

// report prints err once and returns its exit code. In JSON mode the error
// envelope goes to stdout; otherwise a message goes to stderr. Colour is
// downsampled by lipgloss, so pipes and NO_COLOR get plain text.
func report(err error, asJSON bool, stdout, stderr io.Writer) int {
	var es exitStatus
	if errors.As(err, &es) {
		return es.code
	}
	var e *Error
	var apiErr *api.Error
	switch {
	case errors.As(err, &e):
	case errors.As(err, &apiErr):
		// Worker failures keep the Worker's code so agents can branch on it.
		// needs_human is exit 4 wherever it comes from: an agent must stop
		// and hand the change to a person, not retry.
		e = &Error{Code: apiErr.Code, Message: err.Error(), Exit: ExitError}
		if apiErr.Code == api.CodeNeedsHuman {
			e.Exit = ExitNeedsHuman
		}
	default:
		e = &Error{Code: CodeError, Message: err.Error(), Exit: ExitError}
		if errors.Is(err, context.Canceled) {
			e.Code = CodeCanceled
		}
	}
	if e.Exit == ExitOK {
		e.Exit = ExitError // an error never exits 0, whatever built it
	}
	if asJSON {
		if werr := writeEnvelope(stdout, Envelope{V: EnvelopeVersion, OK: false, Error: &ErrorBody{Code: e.Code, Message: e.Message, Details: e.Details}}); werr != nil {
			return ExitError
		}
		return e.Exit
	}
	_, _ = lipgloss.Fprintln(stderr, errorLabel.Render("error:")+" "+e.Message)
	if e.Code == CodeUsage {
		_, _ = fmt.Fprintln(stderr, "Run 'wayglass --help' for usage.")
	}
	return e.Exit
}
