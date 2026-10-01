package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// terminalView owns disk-backed transcripts and a byte-offset index of displayed
// rows. Only the visible rows are read into memory when painting. Raw records
// retain their destination and bytes independently of display sanitization.
type terminalView struct {
	raw, display       *os.File
	rawEnd, displayEnd int64
	rows               []viewRow
	width, height, top int
	follow             bool
	frame              []string
	escape             int
	pending            []byte
	color              bool
	csi                string
	style              map[int]string
	styles             []displayStyle
}

type viewRow struct {
	start, end int64
	cells      int
}

func newTerminalView(width, height int) (*terminalView, error) {
	v := &terminalView{width: width - 1, height: height, follow: true, color: true, rows: []viewRow{{}}}
	var err error
	v.raw, err = os.CreateTemp("", "go-upgrade-transcript-*")
	if err != nil {
		return nil, err
	}
	v.display, err = os.CreateTemp("", "go-upgrade-display-*")
	if err != nil {
		v.dispose()
		return nil, err
	}
	return v, nil
}

func (v *terminalView) dispose() {
	for _, f := range []*os.File{v.raw, v.display} {
		if f != nil {
			name := f.Name()
			f.Close()
			os.Remove(name)
		}
	}
}

func (v *terminalView) capture(data []byte, stderr bool) error {
	var header [9]byte
	if stderr {
		header[0] = 1
	}
	binary.LittleEndian.PutUint64(header[1:], uint64(len(data)))
	if _, err := v.raw.WriteAt(header[:], v.rawEnd); err != nil {
		return err
	}
	if _, err := v.raw.WriteAt(data, v.rawEnd+9); err != nil {
		return err
	}
	v.rawEnd += 9 + int64(len(data))
	return nil
}

func (v *terminalView) replay(out, errOut io.Writer) error {
	r := io.NewSectionReader(v.raw, 0, v.rawEnd)
	for {
		var header [9]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		w := out
		if header[0] == 1 {
			w = errOut
		}
		if _, err := io.CopyN(w, r, int64(binary.LittleEndian.Uint64(header[1:]))); err != nil {
			return err
		}
	}
}

// Preserve SGR styling across write boundaries, while stripping cursor controls
// and OSC/DCS strings. Logs cannot address the screen or change terminal modes.
func (v *terminalView) appendDisplay(data []byte) error {
	data = append(v.pending, data...)
	v.pending = nil
	var clean strings.Builder
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			v.pending = append(v.pending, data...)
			break
		}
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		switch v.escape {
		case 1:
			switch r {
			case '[':
				v.escape, v.csi = 2, ""
			case ']', 'P', '^', '_', 'X':
				v.escape = 3
			default:
				v.escape = 0
			}
			continue
		case 2:
			if r >= 0x40 && r <= 0x7e {
				if r == 'm' && v.csi != "!" {
					v.setStyle(v.csi, v.displayEnd+int64(clean.Len()))
				}
				v.escape = 0
			} else if v.csi != "!" && len(v.csi) < 128 && (r >= '0' && r <= '9' || r == ';') {
				v.csi += string(r)
			} else {
				v.csi = "!"
			}
			continue
		case 3:
			if r == '\a' || r == 0x9c {
				v.escape = 0
			} else if r == '\x1b' {
				v.escape = 4
			}
			continue
		case 4:
			if r == '\\' {
				v.escape = 0
			} else {
				v.escape = 3
			}
			continue
		}
		switch r {
		case '\x1b':
			v.escape = 1
		case 0x9b:
			v.escape, v.csi = 2, ""
		case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
			v.escape = 3
		case '\n':
			clean.WriteByte('\n')
		case '\t':
			clean.WriteString("    ")
		default:
			if !unicode.IsControl(r) {
				clean.WriteRune(r)
			}
		}
	}
	text := clean.String()
	if _, err := v.display.WriteAt([]byte(text), v.displayEnd); err != nil {
		return err
	}
	for _, r := range text {
		v.indexRune(r)
	}
	return nil
}

func (v *terminalView) indexRune(r rune) {
	size := int64(utf8.RuneLen(r))
	last := &v.rows[len(v.rows)-1]
	if r == '\n' {
		v.displayEnd += size
		v.rows = append(v.rows, viewRow{start: v.displayEnd, end: v.displayEnd})
		return
	}
	cells := runeCells(r)
	if last.cells+cells > v.width || last.end-last.start >= 16*1024 {
		v.rows = append(v.rows, viewRow{start: v.displayEnd, end: v.displayEnd})
		last = &v.rows[len(v.rows)-1]
	}
	v.displayEnd += size
	last.end, last.cells = v.displayEnd, last.cells+cells
}

// Combining characters occupy no additional cells; wide CJK and emoji occupy
// two. Using the same measure for wrapping and padding avoids terminal autowrap.
func runeCells(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == '\u200d' {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f || r >= 0xac00 && r <= 0xd7a3 ||
		r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe19 || r >= 0xfe30 && r <= 0xfe6f ||
		r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 ||
		r >= 0x1f000 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

func paddedLine(text string, width int) string {
	var b strings.Builder
	cells := 0
	for _, r := range safeLine(text) {
		n := runeCells(r)
		if cells+n > width {
			break
		}
		b.WriteRune(r)
		cells += n
	}
	b.WriteString(strings.Repeat(" ", width-cells))
	return b.String()
}

func (v *terminalView) rowCount() int {
	n := len(v.rows)
	if n > 0 && v.rows[n-1].start == v.rows[n-1].end {
		n--
	}
	return n
}

func (v *terminalView) bottom() int { return max(0, v.rowCount()-(v.height-3)) }

func (v *terminalView) scroll(delta int) {
	if v.follow {
		v.top = v.bottom()
	}
	v.top = min(v.bottom(), max(0, v.top+delta))
	v.follow = v.top == v.bottom()
}

func (v *terminalView) resize(width, height int) error {
	width-- // Keep the final column clear to avoid pending autowrap.
	if width == v.width && height == v.height {
		return nil
	}
	anchor := v.rows[min(v.top, len(v.rows)-1)].start
	v.height, v.frame = height, nil
	if width != v.width {
		v.width = width
		r := bufio.NewReader(io.NewSectionReader(v.display, 0, v.displayEnd))
		v.rows, v.displayEnd = []viewRow{{}}, 0
		for {
			ch, _, err := r.ReadRune()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			v.indexRune(ch)
		}
		v.top = max(0, sort.Search(len(v.rows), func(i int) bool { return v.rows[i].start > anchor })-1)
	}
	v.top = min(v.top, v.bottom())
	return nil
}

const scrollHintPrefix = "Scrolled up | "
const bottomButton = "[Go to bottom]"
const scrollHint = scrollHintPrefix + bottomButton + "  End / G"

func (v *terminalView) clickBottom(x, y int) {
	// Only the visible button is clickable; log text and the footer are not.
	if !v.follow && len(v.frame) == v.height &&
		strings.HasPrefix(v.frame[v.height-3], scrollHint) &&
		y == v.height-2 && x > len(scrollHintPrefix) && x <= len(scrollHintPrefix)+len(bottomButton) {
		v.follow = true
	}
}

func (v *terminalView) paint(out io.Writer, footer [2]string) error {
	if v.follow {
		v.top = v.bottom()
	}
	frame := make([]string, v.height)
	for i := 0; i < v.height-3; i++ {
		row := v.top + i
		if row < v.rowCount() {
			entry := v.rows[row]
			data := make([]byte, entry.end-entry.start)
			if _, err := v.display.ReadAt(data, entry.start); err != nil {
				return err
			}
			frame[i] = v.styledRow(entry, string(data))
		}
	}
	if !v.follow {
		frame[v.height-3] = scrollHint
	}
	frame[v.height-2], frame[v.height-1] = footer[0], footer[1]
	var update strings.Builder
	for i := range frame {
		if i >= v.height-3 || frame[i] == "" {
			frame[i] = paddedLine(frame[i], v.width)
		}
		if i >= len(v.frame) || frame[i] != v.frame[i] {
			fmt.Fprintf(&update, "\x1b[%d;1H%s\x1b[K", i+1, frame[i])
		}
	}
	if update.Len() > 0 {
		if _, err := io.WriteString(out, update.String()); err != nil {
			return err
		}
	}
	v.frame = frame
	return nil
}
