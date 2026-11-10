package escpos

import (
	"fmt"
	"strings"
)

// Decode renders an ESC/POS byte stream as plain text, the way the paper would read: commands it knows are dropped or
// shown as markers ([CUT], [LACI], [QR ...]), double width text is shown as is. It is what the printer simulator shows
// and what tests compare. Unknown ESC or GS commands become [ESC x] / [GS x] so nothing is silently lost.
func Decode(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch c {
		case esc:
			if i+1 >= len(b) {
				return out.String()
			}
			switch b[i+1] {
			case '@':
				i++
			case 'a', 'E', '!', 'M', '-', 'd':
				i += 2
			case 'p':
				out.WriteString("[LACI]\n")
				i += 4
			default:
				fmt.Fprintf(&out, "[ESC %q]", b[i+1])
				i++
			}
		case gs:
			if i+1 >= len(b) {
				return out.String()
			}
			switch b[i+1] {
			case '!':
				i += 2
			case 'V':
				out.WriteString("[POTONG]\n")
				if i+2 < len(b) && (b[i+2] == 65 || b[i+2] == 66) {
					i += 3
				} else {
					i += 2
				}
			case '(':
				// GS ( k pL pH cn fn [data]
				if i+4 >= len(b) {
					return out.String()
				}
				n := int(b[i+3]) + int(b[i+4])*256
				end := i + 5 + n
				if end > len(b) {
					end = len(b)
				}
				body := b[i+5 : end]
				if len(body) >= 3 && body[0] == 49 && body[1] == 80 {
					fmt.Fprintf(&out, "[QR %s]", string(body[3:]))
				}
				i = end - 1
			default:
				fmt.Fprintf(&out, "[GS %q]", b[i+1])
				i++
			}
		case lf:
			out.WriteByte('\n')
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
