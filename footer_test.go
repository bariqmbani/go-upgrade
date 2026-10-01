package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A small terminal screen model checks the visible rows and normal scrollback,
// instead of merely asserting that particular ANSI sequences were written.
type testScreen struct {
	width, height, row, column int
	cells                      [][]rune
	history                    []string
	saved                      *testScreen
}

func newTestScreen(width, height int) *testScreen {
	s := &testScreen{width: width, height: height}
	for range height {
		s.cells = append(s.cells, make([]rune, width))
	}
	return s
}

func (s *testScreen) newline() {
	s.column = 0 // The Unix TTY applies ONLCR to output newlines.
	s.row++
	if s.row == s.height {
		s.history = append(s.history, s.line(0))
		s.cells = append(s.cells[1:], make([]rune, s.width))
		s.row--
	}
}

func (s *testScreen) feed(data string) {
	for len(data) > 0 {
		if strings.HasPrefix(data, "\x1b[") {
			end := 2
			for end < len(data) && (data[end] >= '0' && data[end] <= '9' || data[end] == ';' || data[end] == '?') {
				end++
			}
			if end >= len(data) {
				panic("incomplete terminal sequence")
			}
			params := strings.Split(data[2:end], ";")
			value := func(i, fallback int) int {
				if i >= len(params) || params[i] == "" {
					return fallback
				}
				n, _ := strconv.Atoi(params[i])
				return n
			}
			switch data[end] {
			case 'H':
				s.row = min(s.height-1, max(0, value(0, 1)-1))
				s.column = min(s.width-1, max(0, value(1, 1)-1))
			case 'K':
				start := s.column
				if value(0, 0) == 2 {
					start = 0
				}
				for col := start; col < s.width; col++ {
					s.cells[s.row][col] = 0
				}
			case 'h', 'l':
				if data[2:end] == "?1049" {
					if data[end] == 'h' {
						old := *s
						*s = *newTestScreen(s.width, s.height)
						s.saved = &old
					} else if s.saved != nil {
						*s = *s.saved
					}
				}
			case 'm': // Styling does not change screen geometry.
			default:
				panic("unexpected terminal control: " + data[:end+1])
			}
			data = data[end+1:]
			continue
		}
		char := data[0]
		data = data[1:]
		switch char {
		case '\n':
			s.newline()
		case '\r':
			s.column = 0
		default:
			if s.column == s.width {
				s.newline()
			}
			s.cells[s.row][s.column] = rune(char)
			s.column++
		}
	}
}

func (s *testScreen) line(row int) string {
	return strings.TrimRight(strings.ReplaceAll(string(s.cells[row]), "\x00", " "), " ")
}

func (s *testScreen) transcript() string {
	lines := append([]string(nil), s.history...)
	for row := range s.height {
		lines = append(lines, s.line(row))
	}
	return strings.Join(lines, "\n")
}

func TestFixedFooterPreservesLogsAndMargin(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, false)
	t.Cleanup(p.close)
	p.configure(true)
	p.terminalSize = func() (int, int, bool) { return 100, 12, true }
	screen := newTestScreen(100, 12)
	flush := func() {
		screen.feed(out.String())
		out.Reset()
	}
	paint := func() {
		p.mu.Lock()
		p.renderLocked(time.Now())
		p.mu.Unlock()
		flush()
	}
	p.text("HEADER\n")
	p.beginMetadata(4, 2)
	p.lookup(lookupEvent{"metadata", "example.com/a@v1.2.3", cacheShared})
	p.moduleDone("example.com/done")
	paint()
	assertFooter := func() {
		t.Helper()
		if screen.line(9) != "" || !strings.Contains(screen.line(10), "Metadata 1/4 (25%)") || !strings.Contains(screen.line(10), "~45%") || !strings.Contains(screen.line(11), "example.com/a@v1.2.3 | Cached (shared)") {
			t.Fatalf("wrong footer rows:\n%s", screen.transcript())
		}
	}
	assertFooter()
	for i := range 40 {
		p.status("OK", "PERMANENT-%02d", i)
		flush()
		assertFooter() // Log writes must not blank the footer between ticks.
		paint()
		assertFooter()
	}
	p.diagnostic("Long error", strings.Repeat("diagnostic detail\n", 20)+"ERROR-LAST\n")
	flush()
	assertFooter()
	paint()
	assertFooter()
	p.close()
	flush()
	transcript := screen.transcript()
	for i := range 40 {
		if !strings.Contains(transcript, fmt.Sprintf("PERMANENT-%02d", i)) {
			t.Fatalf("lost permanent log %d", i)
		}
	}
	if !strings.Contains(transcript, "HEADER") || !strings.Contains(transcript, "ERROR-LAST") {
		t.Fatal("normal scrollback lost header or error diagnostics")
	}
	p.close()
	flush()
	if strings.Contains(screen.transcript(), "Overall [") || p.footerHeight != 0 {
		t.Fatal("footer remained after exit")
	}
}

func TestActiveLookupDoesNotShowFinishedDependency(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, false)
	p.beginMetadata(2, 2)
	p.lookup(lookupEvent{"metadata", "example.com/slow@v1.0.0", cacheMiss})
	p.lookup(lookupEvent{"metadata", "example.com/cached@v1.0.0", cacheShared})
	p.moduleDone("example.com/cached")
	if p.progress.item != "example.com/slow@v1.0.0" || p.progress.source != string(cacheMiss) {
		t.Fatalf("finished dependency still displayed: %+v", p.progress)
	}
}

func TestFooterResizeAndSmallTerminalFallback(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, false)
	width, height := 100, 24
	p.terminalSize = func() (int, int, bool) { return width, height, width >= 60 && height >= 8 }
	p.beginMetadata(8, 4)
	paint := func() {
		p.mu.Lock()
		p.renderLocked(time.Now())
		p.mu.Unlock()
	}
	paint()
	out.Reset()
	width, height = 72, 16
	paint()
	if p.footerHeight != 16 || !strings.Contains(out.String(), "\x1b[15;1HOverall") || !strings.Contains(out.String(), "\x1b[16;1H") {
		t.Fatalf("footer did not move to resized terminal bottom: %q", out.String())
	}
	width, height = 40, 6
	paint()
	if p.footerHeight != 0 {
		t.Fatal("footer did not release on an undersized terminal")
	}
	out.Reset()
	width, height = 100, 24
	paint() // Fallback is permanent, even if the terminal becomes usable again.
	p.text("Plain output\n")
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("terminal controls in fallback output: %q", out.String())
	}
}
