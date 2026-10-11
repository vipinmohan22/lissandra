package ingest

import (
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// utf16Reader transcodes UTF-16 (as written by Windows PowerShell and
// Event Viewer) to UTF-8 on the fly.
type utf16Reader struct {
	r         io.Reader
	bigEndian bool
	in        []byte // undecoded input carried between reads (< 4 bytes)
	out       []byte // decoded output not yet returned
	buf       []byte
	err       error
}

func newUTF16Reader(r io.Reader, bigEndian bool) io.Reader {
	return &utf16Reader{r: r, bigEndian: bigEndian, buf: make([]byte, 32<<10)}
}

func (u *utf16Reader) Read(p []byte) (int, error) {
	for len(u.out) == 0 {
		if u.err != nil {
			return 0, u.err
		}
		n, err := u.r.Read(u.buf)
		u.in = append(u.in, u.buf[:n]...)
		u.err = err
		u.decode(err != nil)
	}
	n := copy(p, u.out)
	u.out = u.out[n:]
	return n, nil
}

// decode converts complete code units (and pairs) in u.in to UTF-8. At the
// end of input, a dangling byte or lone surrogate becomes U+FFFD.
func (u *utf16Reader) decode(final bool) {
	i := 0
	unit := func(j int) uint16 {
		if u.bigEndian {
			return uint16(u.in[j])<<8 | uint16(u.in[j+1])
		}
		return uint16(u.in[j+1])<<8 | uint16(u.in[j])
	}
	var tmp [utf8.UTFMax]byte
	for i+1 < len(u.in) {
		c := unit(i)
		r := rune(c)
		size := 2
		if utf16.IsSurrogate(r) {
			if i+3 >= len(u.in) {
				if !final {
					break // wait for the rest of the pair
				}
				r = utf8.RuneError
			} else if d := utf16.DecodeRune(r, rune(unit(i+2))); d != utf8.RuneError {
				r, size = d, 4
			} else {
				r = utf8.RuneError
			}
		}
		n := utf8.EncodeRune(tmp[:], r)
		u.out = append(u.out, tmp[:n]...)
		i += size
	}
	u.in = append(u.in[:0], u.in[i:]...)
	if final && len(u.in) > 0 {
		u.out = utf8.AppendRune(u.out, utf8.RuneError)
		u.in = u.in[:0]
	}
}
