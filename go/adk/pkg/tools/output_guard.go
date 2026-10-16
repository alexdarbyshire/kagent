package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Each stream retains at most 8 KiB, regardless of line boundaries or exit status.
const commandStreamBytes = 8 * 1024

type commandOutput struct {
	data    []byte
	omitted int64
}

func (b *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, commandStreamBytes-len(b.data))
	b.data = append(b.data, p[:keep]...)
	b.omitted += int64(n - keep)
	// Drain excess bytes so clipping does not turn success into a broken pipe.
	return n, nil
}

func (b *commandOutput) String() string {
	data := b.data
	for len(data) > 0 {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size != 1 {
			break
		}
		data = data[:len(data)-1]
	}
	text := strings.ToValidUTF8(string(data), "?")
	if b.omitted > 0 {
		text += fmt.Sprintf("\n[output truncated: at least %d bytes omitted; save output to file and inspect bounded excerpts]", b.omitted)
	}
	return text
}
