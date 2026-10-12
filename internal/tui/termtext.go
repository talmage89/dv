package tui

import (
	"strings"
)

// outBuf accumulates a command's output as display lines. It keeps color
// (SGR) escapes, drops every other control sequence so cursor movement cannot
// corrupt the layout, and treats a bare carriage return as "rewrite the
// current line" so progress bars collapse to their final state.
type outBuf struct {
	lines   []string
	partial strings.Builder
	esc     []byte // an escape sequence split across writes
	dropped int    // lines discarded from the front to respect maxLines
}

const maxOutLines = 10000

func (o *outBuf) write(p []byte) {
	data := p
	if len(o.esc) > 0 {
		data = append(append([]byte(nil), o.esc...), p...)
		o.esc = nil
	}
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '\n':
			o.newline()
		case c == '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				continue // CRLF: the \n ends the line
			}
			if i+1 == len(data) {
				continue // can't tell yet; a following \n would make it CRLF
			}
			o.partial.Reset()
		case c == 0x1b:
			n, keep, complete := scanEscape(data[i:])
			if !complete {
				o.esc = append([]byte(nil), data[i:]...)
				return
			}
			if keep {
				o.partial.Write(data[i : i+n])
			}
			i += n - 1
		case c == '\t':
			o.partial.WriteString("    ")
		case c < 0x20 || c == 0x7f:
			// other control characters (bell, backspace, ...) are dropped
		default:
			o.partial.WriteByte(c)
		}
	}
}

func (o *outBuf) newline() {
	o.lines = append(o.lines, o.partial.String())
	o.partial.Reset()
	if len(o.lines) > maxOutLines {
		cut := len(o.lines) - maxOutLines
		o.lines = append([]string(nil), o.lines[cut:]...)
		o.dropped += cut
	}
}

// all returns every line including the unterminated last one.
func (o *outBuf) all() []string {
	if o.partial.Len() == 0 {
		return o.lines
	}
	return append(o.lines[:len(o.lines):len(o.lines)], o.partial.String())
}

// scanEscape measures the escape sequence at the start of b. keep is true for
// SGR (color) sequences. complete is false when b ends mid-sequence.
func scanEscape(b []byte) (n int, keep, complete bool) {
	if len(b) < 2 {
		return 0, false, false
	}
	switch b[1] {
	case '[': // CSI: parameters, then a final byte in 0x40-0x7e
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1, b[i] == 'm', true
			}
		}
		return 0, false, false
	case ']', 'P', '_', '^': // OSC/DCS/APC/PM: until BEL or ST (ESC \)
		for i := 2; i < len(b); i++ {
			if b[i] == 0x07 {
				return i + 1, false, true
			}
			if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2, false, true
			}
		}
		return 0, false, false
	default: // two-byte sequence
		return 2, false, true
	}
}
