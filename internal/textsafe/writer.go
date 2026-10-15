package textsafe

import (
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	ground  = iota
	escape  // saw ESC
	csi     // inside ESC [ ... final
	str     // inside OSC, DCS, SOS, PM or APC: dropped until BEL, ST or newline
	strEsc  // saw ESC inside a string: ESC \ ends it
	csiSkip // inside an overlong control sequence: dropped to its end
)

// maxCSI bounds a control sequence; anything longer is dropped.
const maxCSI = 64

// Writer filters a terminal stream: it drops every escape sequence except
// SGR (colour and style: ESC [ ... m, without conceal), every unsafe rune
// and invalid UTF-8. Wayglass's own styling is SGR only, so the output looks
// the same; text written by others can no longer move the cursor, erase,
// set titles or hyperlinks, or touch the clipboard. A string sequence that
// never ends is cut at the next newline, so it cannot swallow the rest of
// the output.
//
// Use NewWriter. The zero value is not usable.
type Writer struct {
	w     io.Writer
	sgr   bool // keep SGR sequences
	state int
	seq   []byte
	pend  []byte // an incomplete UTF-8 sequence from the last Write
	out   []byte
}

// NewWriter returns a filtering writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, sgr: true} }

// Unwrap returns the writer underneath.
func (f *Writer) Unwrap() io.Writer { return f.w }

// Write filters p and writes the result. It reports len(p) on success:
// dropped bytes count as written.
func (f *Writer) Write(p []byte) (int, error) {
	data := p
	if len(f.pend) > 0 {
		data = append(f.pend, p...)
		f.pend = nil
	}
	f.out = f.out[:0]
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 && !utf8.FullRune(data[i:]) {
			f.pend = append([]byte(nil), data[i:]...)
			break
		}
		f.step(r, data[i:i+size])
		i += size
	}
	if len(f.out) > 0 {
		if _, err := f.w.Write(f.out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush ends the stream: a pending partial rune is replaced, an open
// sequence dropped.
func (f *Writer) flush() {
	if len(f.pend) > 0 {
		_, _ = io.WriteString(f.w, string(utf8.RuneError))
		f.pend = nil
	}
	f.state, f.seq = ground, nil
}

func (f *Writer) step(r rune, raw []byte) {
	switch f.state {
	case escape:
		switch {
		case r == '[' && len(f.seq) == 1:
			f.state, f.seq = csi, append(f.seq, raw...)
		case strings.ContainsRune("]PX^_", r) && len(f.seq) == 1:
			f.state, f.seq = str, nil
		case r == 0x1b:
			f.seq = append(f.seq[:0], raw...) // a fresh ESC replaces the first
		case r >= 0x20 && r <= 0x2f && len(f.seq) < 8:
			// An intermediate (ESC ( B, ESC # 8): the final byte follows.
			f.seq = append(f.seq, raw...)
		default:
			f.state, f.seq = ground, nil // the final byte: the escape is dropped
			if r == '\n' {
				f.out = append(f.out, '\n')
			}
		}
	case csi:
		switch {
		case r >= 0x40 && r <= 0x7e: // final byte
			if r == 'm' && f.sgr && sgrAllowed(string(f.seq[2:])) {
				f.out = append(f.out, f.seq...)
				f.out = append(f.out, 'm')
			}
			f.state, f.seq = ground, nil
		case r >= 0x20 && r <= 0x3f && len(f.seq) < maxCSI:
			f.seq = append(f.seq, raw...)
		case r >= 0x20 && r <= 0x3f:
			f.state, f.seq = csiSkip, nil
		default: // malformed or too long: drop it and look at r afresh
			f.state, f.seq = ground, nil
			f.ground(r, raw)
		}
	case csiSkip:
		if r < 0x20 || r > 0x3f {
			f.state = ground
			if r < 0x40 || r > 0x7e {
				f.ground(r, raw)
			}
		}
	case str:
		switch r {
		case 0x07:
			f.state = ground
		case 0x1b:
			f.state = strEsc
		case '\n':
			f.state = ground
			f.out = append(f.out, '\n')
		}
	case strEsc:
		switch r {
		case '\\':
			f.state = ground
		case '\n':
			f.state = ground
			f.out = append(f.out, '\n')
		case 0x1b:
		default:
			f.state = str
		}
	default:
		f.ground(r, raw)
	}
}

func (f *Writer) ground(r rune, raw []byte) {
	switch {
	case r == 0x1b:
		f.state, f.seq = escape, append(f.seq[:0], raw...)
	case r == utf8.RuneError && len(raw) == 1:
		f.out = append(f.out, string(utf8.RuneError)...)
	case unsafeRune(r):
	default:
		f.out = append(f.out, raw...)
	}
}

// sgrAllowed accepts SGR parameters made of numbers, ';' and ':' only, and
// refuses conceal (8), which would hide text from the reader.
func sgrAllowed(params string) bool {
	if params == "" {
		return true
	}
	for _, c := range params {
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		if strings.Contains(p, ":") { // colon form, e.g. 38:2::r:g:b, is self-contained
			continue
		}
		n, err := strconv.Atoi(p)
		if p != "" && err != nil {
			return false
		}
		switch n {
		case 8:
			return false
		case 38, 48, 58: // extended colour: skip its arguments
			if i+1 < len(ps) {
				switch ps[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
		}
	}
	return true
}
