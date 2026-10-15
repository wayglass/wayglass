package cli

import (
	"io"
	"os"

	"github.com/wayglass/wayglass/internal/textsafe"
)

// Everything a command prints can carry text written by others: goals,
// reasons, agent names, file paths, audit summaries, git notes. safeStream
// filters stdout and stderr so that text cannot drive the terminal
// (textsafe.Writer keeps Wayglass's own colour). The TUI draws with cursor
// movement and gets the raw stream instead (rawStream), after cleaning its
// data in internal/tui.

// safeFile is a filtered *os.File. Embedding keeps Fd and Stat, so terminal
// and colour detection still see the real file.
type safeFile struct {
	*os.File
	f *textsafe.Writer
}

func (s safeFile) Write(p []byte) (int, error) { return s.f.Write(p) }

// WriteString overrides the embedded *os.File method, which would bypass
// the filter.
func (s safeFile) WriteString(v string) (int, error) { return s.f.Write([]byte(v)) }

func safeStream(w io.Writer) io.Writer {
	switch v := w.(type) {
	case safeFile, *textsafe.Writer:
		return w
	case *os.File:
		return safeFile{File: v, f: textsafe.NewWriter(v)}
	}
	return textsafe.NewWriter(w)
}

// rawStream undoes safeStream, for the TUI renderer.
func rawStream(w io.Writer) io.Writer {
	switch v := w.(type) {
	case safeFile:
		return v.File
	case *textsafe.Writer:
		return v.Unwrap()
	}
	return w
}

// osFile returns the terminal file behind w, filtered or not.
func osFile(w any) (*os.File, bool) {
	switch v := w.(type) {
	case *os.File:
		return v, true
	case safeFile:
		return v.File, true
	}
	return nil, false
}
