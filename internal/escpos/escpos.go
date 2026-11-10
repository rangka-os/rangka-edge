// Package escpos builds ESC/POS byte streams for thermal receipt printers (Epson TM, and the common 58 mm and 80 mm
// printers that follow the same command set) and decodes them back to text for the printer simulator and tests.
//
// Only the commands every ESC/POS printer understands are used: initialise, alignment, emphasis, character size,
// line feeds, QR code (GS ( k, model 2), partial cut and the drawer kick. Text goes out as printable ASCII: the
// printer's default code page differs between models, so letters outside ASCII are folded to their base letter and
// anything else becomes '?'. Receipts in Bahasa Indonesia need nothing more.
package escpos

import (
	"bytes"
	"strings"
)

// Columns per line in font A: 48 on 80 mm paper (576 dots / 12), 32 on 58 mm (384 dots / 12).
const (
	Cols80 = 48
	Cols58 = 32
)

// Align is a justification for ESC a.
type Align byte

const (
	Left   Align = 0
	Center Align = 1
	Right  Align = 2
)

const (
	esc = 0x1b
	gs  = 0x1d
	lf  = 0x0a
)

// Builder accumulates one print job. The zero value is not usable: use New.
type Builder struct {
	buf  bytes.Buffer
	cols int
	wide bool
}

// New starts a job for a printer with cols columns in font A and initialises the printer (ESC @).
func New(cols int) *Builder {
	if cols <= 0 {
		cols = Cols80
	}
	b := &Builder{cols: cols}
	b.buf.Write([]byte{esc, '@'})
	return b
}

// Cols is the number of characters that fit on one line at the current size.
func (b *Builder) Cols() int {
	if b.wide {
		return b.cols / 2
	}
	return b.cols
}

// Align sets the justification of the following lines.
func (b *Builder) Align(a Align) *Builder {
	b.buf.Write([]byte{esc, 'a', byte(a)})
	return b
}

// Bold turns emphasis on or off.
func (b *Builder) Bold(on bool) *Builder {
	b.buf.Write([]byte{esc, 'E', boolByte(on)})
	return b
}

// Size sets double width and/or double height (GS !). Double width halves the columns per line.
func (b *Builder) Size(doubleWidth, doubleHeight bool) *Builder {
	var n byte
	if doubleWidth {
		n |= 0x10
	}
	if doubleHeight {
		n |= 0x01
	}
	b.wide = doubleWidth
	b.buf.Write([]byte{gs, '!', n})
	return b
}

// Line prints text and a line feed. Text longer than a line wraps at spaces (or hard, for one long word).
func (b *Builder) Line(text string) *Builder {
	for _, l := range Wrap(Clean(text), b.Cols()) {
		b.buf.WriteString(l)
		b.buf.WriteByte(lf)
	}
	return b
}

// Pair prints left and right on one line, right flush to the margin (an amount next to its label). When both do not
// fit, left wraps and right goes on its own line(s), flush right.
func (b *Builder) Pair(left, right string) *Builder {
	left, right = Clean(left), Clean(right)
	w := b.Cols()
	if len(left)+1+len(right) <= w {
		b.buf.WriteString(left + strings.Repeat(" ", w-len(left)-len(right)) + right)
		b.buf.WriteByte(lf)
		return b
	}
	for _, l := range Wrap(left, w) {
		b.buf.WriteString(l)
		b.buf.WriteByte(lf)
	}
	for _, r := range Wrap(right, w) {
		if r == "" {
			continue
		}
		b.buf.WriteString(strings.Repeat(" ", w-len(r)) + r)
		b.buf.WriteByte(lf)
	}
	return b
}

// Rule prints a full-width line of ch ('-' or '=').
func (b *Builder) Rule(ch byte) *Builder {
	b.buf.WriteString(strings.Repeat(string(ch), b.Cols()))
	b.buf.WriteByte(lf)
	return b
}

// Feed prints n empty lines.
func (b *Builder) Feed(n int) *Builder {
	for i := 0; i < n; i++ {
		b.buf.WriteByte(lf)
	}
	return b
}

// QR prints data as a QR code (model 2, module size 6, error correction M), centred. Empty data or data over
// 700 bytes prints nothing.
func (b *Builder) QR(data string) *Builder {
	if data == "" || len(data) > 700 {
		return b
	}
	n := len(data) + 3
	b.Align(Center)
	b.buf.Write([]byte{gs, '(', 'k', 4, 0, 49, 65, 50, 0}) // model 2
	b.buf.Write([]byte{gs, '(', 'k', 3, 0, 49, 67, 6})     // module size
	b.buf.Write([]byte{gs, '(', 'k', 3, 0, 49, 69, 49})    // error correction M
	b.buf.Write([]byte{gs, '(', 'k', byte(n % 256), byte(n / 256), 49, 80, 48})
	b.buf.WriteString(data)
	b.buf.Write([]byte{gs, '(', 'k', 3, 0, 49, 81, 48}) // print
	b.buf.WriteByte(lf)
	return b
}

// Cut feeds past the tear bar and cuts the paper, leaving a small hinge (GS V 66 n).
func (b *Builder) Cut() *Builder {
	b.buf.Write([]byte{gs, 'V', 66, 3})
	return b
}

// OpenDrawer kicks the cash drawer on pin 2 (ESC p 0, 25 ms on, 250 ms off).
func (b *Builder) OpenDrawer() *Builder {
	b.buf.Write([]byte{esc, 'p', 0, 25, 250})
	return b
}

// Bytes is the job so far.
func (b *Builder) Bytes() []byte { return b.buf.Bytes() }

func boolByte(on bool) byte {
	if on {
		return 1
	}
	return 0
}

// fold maps the Latin letters with accents that show up in names and menus to their base letter.
var fold = map[rune]byte{}

func init() {
	for base, set := range map[byte]string{
		'a': "àáâãäåā", 'A': "ÀÁÂÃÄÅĀ", 'c': "çć", 'C': "ÇĆ", 'e': "èéêëē", 'E': "ÈÉÊËĒ", 'i': "ìíîïī", 'I': "ÌÍÎÏĪ",
		'n': "ñ", 'N': "Ñ", 'o': "òóôõöøō", 'O': "ÒÓÔÕÖØŌ", 'u': "ùúûüū", 'U': "ÙÚÛÜŪ", 'y': "ýÿ", 'Y': "Ý",
	} {
		for _, r := range set {
			fold[r] = base
		}
	}
}

// Clean folds text to printable ASCII: accents dropped (é -> e), tabs and newlines become spaces, other runes '?'.
func Clean(s string) string {
	var out strings.Builder
	for _, r := range s {
		if c, ok := fold[r]; ok {
			out.WriteByte(c)
			continue
		}
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			out.WriteByte(' ')
		case r == '–' || r == '—':
			out.WriteByte('-')
		case r == '‘' || r == '’':
			out.WriteByte('\'')
		case r == '“' || r == '”':
			out.WriteByte('"')
		case r >= 0x20 && r < 0x7f:
			out.WriteRune(r)
		default:
			out.WriteByte('?')
		}
	}
	return out.String()
}

// Wrap splits ASCII text into lines of at most w characters, breaking at spaces where it can.
func Wrap(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	if s == "" {
		return []string{""}
	}
	var out []string
	for len(s) > w {
		cut := strings.LastIndexByte(s[:w+1], ' ')
		if cut <= 0 {
			out = append(out, s[:w])
			s = s[w:]
			continue
		}
		out = append(out, strings.TrimRight(s[:cut], " "))
		s = strings.TrimLeft(s[cut:], " ")
	}
	return append(out, s)
}
