package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func testView(t *testing.T, width, height int) *terminalView {
	t.Helper()
	v, err := newTerminalView(width, height)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.dispose)
	return v
}

func TestIndependentLogAndFooterRendering(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, false)
	p.terminalSize = func() (int, int, bool) { return 80, 10, true }
	t.Cleanup(p.close)
	p.beginMetadata(10, 2)
	now := p.progress.started.Add(time.Second)
	p.renderLocked(now)
	out.Reset()
	for i := range 1000 {
		p.text("line %d\n", i)
	}
	if out.Len() != 0 {
		t.Fatal("log ingestion wrote to the screen")
	}
	p.renderLocked(now)
	if strings.Contains(out.String(), "\x1b[9;") || strings.Contains(out.String(), "\x1b[10;") {
		t.Fatal("log updates rewrote unchanged footer rows")
	}
	out.Reset()
	p.renderLocked(now)
	if out.Len() != 0 {
		t.Fatal("unchanged frame produced output")
	}
	p.checkDone()
	p.work.advance()
	p.renderLocked(now.Add(time.Second))
	for row := 1; row <= 8; row++ {
		if strings.Contains(out.String(), fmt.Sprintf("\x1b[%d;", row)) {
			t.Fatal("footer update rewrote log pane")
		}
	}
	out.Reset()
	p.endProgress()
	p.beginPhase(stageFinal, "Final", 2)
	if out.Len() != 0 {
		t.Fatal("phase transition erased the footer")
	}
}

func TestViewportScrollAnchorAndResize(t *testing.T) {
	v := testView(t, 80, 10)
	for i := range 40 {
		if err := v.appendDisplay(fmt.Appendf(nil, "line-%02d %s\n", i, strings.Repeat("x", 100))); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	footer := [2]string{"progress", "active"}
	v.paint(&out, footer)
	v.scroll(-10)
	anchor := v.rows[v.top].start
	v.appendDisplay([]byte("new output\n"))
	v.paint(&out, footer)
	if v.rows[v.top].start != anchor || v.follow {
		t.Fatal("new logs moved the scrolled viewport")
	}
	if !strings.Contains(out.String(), "Go to bottom") {
		t.Fatal("missing scroll hint")
	}
	if err := v.resize(60, 8); err != nil {
		t.Fatal(err)
	}
	row := v.rows[v.top]
	if row.start > anchor || row.end < anchor {
		t.Fatal("resize lost the viewed text anchor")
	}
	v.scroll(10000)
	if !v.follow || v.top != v.bottom() {
		t.Fatal("scroll to bottom did not resume following")
	}
}

func TestDisplaySanitizesControlsAndReplaysOriginalBytes(t *testing.T) {
	v := testView(t, 60, 8)
	raw := "first\x1b[2J\x1b[?1049l\x1b]0;TITLE\a\x1bPignored\x1b\\\t世界 e\u0301\nlast\r\x00\a\n"
	for _, b := range []byte(raw) {
		data := []byte{b}
		if err := v.capture(data, false); err != nil {
			t.Fatal(err)
		}
		if err := v.appendDisplay(data); err != nil {
			t.Fatal(err)
		}
	}
	var display bytes.Buffer
	io.Copy(&display, io.NewSectionReader(v.display, 0, v.displayEnd))
	if got := display.String(); got != "first    世界 e\u0301\nlast\n" {
		t.Fatalf("unsafe display: %q", got)
	}
	if err := v.capture([]byte("stderr\n"), true); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := v.replay(&out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != raw || errOut.String() != "stderr\n" {
		t.Fatal("replay changed bytes or routing")
	}
}

func TestInputKeysAndFragmentedMouse(t *testing.T) {
	pending, events := inputEvents("\x1b[A\x1b[B\x1b[5~\x1b[6~\x1b[H\x1b[F\x1b[<64;")
	if strings.Join(events, ",") != "up,down,pageup,pagedown,home,end" {
		t.Fatal(events)
	}
	pending, events = inputEvents(pending + "2;3M\x1b[<65;2;3M\x03")
	if pending != "" || strings.Join(events, ",") != "wheelup,wheeldown,interrupt" {
		t.Fatal(pending, events)
	}
}

func TestRedirectedStderrIsImmediateAndNotReplayed(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newPresenter(&out, &errOut, false)
	p.terminalSize = func() (int, int, bool) { return 80, 10, true }
	p.beginMetadata(1, 1)
	p.diagnostic("warning", "details")
	expected := errOut.String()
	if !strings.Contains(expected, "details") {
		t.Fatal("redirected diagnostic delayed")
	}
	rawName, displayName := p.view.raw.Name(), p.view.display.Name()
	p.close()
	p.close()
	if errOut.String() != expected {
		t.Fatal("redirected stderr replayed twice")
	}
	for _, name := range []string{rawName, displayName} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("spool was not removed")
		}
	}
}

func TestSpoolFailureFallsBackWithoutLosingOutput(t *testing.T) {
	for _, failDisplay := range []bool{false, true} {
		t.Run(fmt.Sprint(failDisplay), func(t *testing.T) {
			var out bytes.Buffer
			p := newPresenter(&out, &out, false)
			p.terminalSize = func() (int, int, bool) { return 80, 10, true }
			t.Cleanup(p.close)
			p.text("before\n")
			if failDisplay {
				p.view.display.Close()
			} else {
				// Reopen read-only: prior records remain readable during fallback.
				name := p.view.raw.Name()
				p.view.raw.Close()
				var err error
				p.view.raw, err = os.Open(name)
				if err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			p.text("after\n")
			if !p.plain || p.view != nil || out.String() != leaveView+"before\nafter\n" {
				t.Fatalf("bad fallback: %q", out.String())
			}
		})
	}
}

// The reader blocks between chunks while the render loop must remain usable.
type pausedTranscript struct {
	read, resume chan struct{}
	sent         bool
}

func (r *pausedTranscript) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "first\n"), nil
	}
	close(r.read)
	<-r.resume
	return 0, io.EOF
}

func TestCommandTranscriptDoesNotHoldRenderLock(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, true)
	p.terminalSize = func() (int, int, bool) { return 80, 10, true }
	t.Cleanup(p.close)
	p.beginMetadata(1, 1)
	r := &pausedTranscript{read: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); p.commandOutput("build", "go build", r, nil) }()
	<-r.read
	painted := make(chan struct{})
	go func() { p.mu.Lock(); p.renderLocked(time.Now()); p.mu.Unlock(); close(painted) }()
	select {
	case <-painted:
	case <-time.After(time.Second):
		close(r.resume)
		<-done
		t.Fatal("command transcript held the render lock")
	}
	close(r.resume)
	<-done
}

func TestSharedTerminalRetainsStreamRouting(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newPresenter(&out, &errOut, false)
	p.errSharesTerminal = true
	p.terminalSize = func() (int, int, bool) { return 80, 10, true }
	p.text("stdout\n")
	p.diagnostic("warning", "stderr")
	if errOut.Len() != 0 {
		t.Fatal("shared-terminal diagnostic bypassed the viewport")
	}
	p.close()
	if strings.Contains(out.String(), "stderr") || !strings.Contains(errOut.String(), "stderr") {
		t.Fatal("shared-terminal replay lost stream routing")
	}
}

func TestIncompleteEscapeDoesNotSwallowInterrupt(t *testing.T) {
	pending, events := inputEvents("\x1b[\x03")
	if pending != "" || len(events) != 1 || events[0] != "interrupt" {
		t.Fatal(pending, events)
	}
}

func TestViewportWrapsWideTextAndBoundsLongLines(t *testing.T) {
	v := testView(t, 61, 10)
	text := strings.Repeat("界", 100) + "\n" + strings.Repeat("x", 100000) + "\n"
	if err := v.appendDisplay([]byte(text)); err != nil {
		t.Fatal(err)
	}
	first := v.rows[0]
	data := make([]byte, first.end-first.start)
	if _, err := v.display.ReadAt(data, first.start); err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.Repeat("界", 30) {
		t.Fatal("wide text wrapped at the wrong column")
	}
	if v.rows[1].start != first.end {
		t.Fatal("wrapping dropped text")
	}
	for _, row := range v.rows {
		if row.cells > 60 || row.end-row.start > 180 {
			t.Fatal("oversized display row")
		}
	}
}

func TestDisplayPreservesColorsAcrossWrappingAndResize(t *testing.T) {
	v := testView(t, 61, 8)
	text := "\x1b[32m" + strings.Repeat("G", 130) + "\x1b[0m\nplain\n"
	for _, b := range []byte(text) {
		if err := v.appendDisplay([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	for _, width := range []int{61, 81} {
		if err := v.resize(width, 8); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := v.paint(&out, [2]string{"footer", "timer"}); err != nil {
			t.Fatal(err)
		}
		for i, row := range v.rows[:v.rowCount()-1] {
			data := make([]byte, row.end-row.start)
			v.display.ReadAt(data, row.start)
			rendered := v.styledRow(row, string(data))
			if !strings.Contains(rendered, "\x1b[0;32m"+string(data)+"\x1b[0m") {
				t.Fatalf("row %d lost its color after wrap/resize: %q", i, rendered)
			}
		}
		if strings.Contains(out.String(), "\x1b[0;32mplain") || strings.Contains(out.String(), "\x1b[0;32mfooter") {
			t.Fatal("color leaked past the styled output")
		}
		v.scroll(-2)
	}
}

func TestDisplayColorsRespectNoColorAndStripScreenControls(t *testing.T) {
	for _, color := range []bool{true, false} {
		v := testView(t, 80, 8)
		v.color = color
		v.appendDisplay([]byte("\x1b[1;38;2;10;20;30mRGB\x1b[0m\x1b[2J\x1b[?1049l\n"))
		var out bytes.Buffer
		v.paint(&out, [2]string{})
		if strings.Contains(out.String(), "\x1b[0;1;38;2;10;20;30m") != color {
			t.Fatalf("wrong color behavior (enabled=%v): %q", color, out.String())
		}
		if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "\x1b[?1049l") {
			t.Fatal("screen controls escaped into viewport")
		}
	}
}

func TestGoToBottomControls(t *testing.T) {
	for _, control := range []string{"\x1b[F", "G", "\x1b[<0;16;8M"} {
		t.Run(control, func(t *testing.T) {
			v := testView(t, 80, 10)
			if err := v.appendDisplay([]byte(strings.Repeat("log\n", 40))); err != nil {
				t.Fatal(err)
			}
			v.scroll(-10)
			var out bytes.Buffer
			footer := [2]string{"progress", "elapsed"}
			if err := v.paint(&out, footer); err != nil {
				t.Fatal(err)
			}
			p := &presenter{view: v}
			// Clicking adjacent rows, outside the button, or releasing a mouse button
			// must leave the viewport anchored.
			for _, input := range []string{"\x1b[<0;1;8M", "\x1b[<0;16;7M", "\x1b[<0;16;9M", "\x1b[<0;30;8M", "\x1b[<0;16;8m"} {
				_, events := inputEvents(input)
				for _, event := range events {
					p.inputLocked(event)
				}
				if v.follow {
					t.Fatalf("unrelated click activated button: %q", input)
				}
			}
			_, events := inputEvents(control)
			for _, event := range events {
				p.inputLocked(event)
			}
			if !v.follow {
				t.Fatal("control did not resume following")
			}
			out.Reset()
			if err := v.paint(&out, footer); err != nil {
				t.Fatal(err)
			}
			if v.top != v.bottom() || strings.Contains(v.frame[7], "Go to bottom") {
				t.Fatal("viewport did not return to bottom")
			}
			if strings.Contains(out.String(), "\x1b[9;") || strings.Contains(out.String(), "\x1b[10;") {
				t.Fatal("returning to bottom rewrote the footer")
			}
		})
	}
}
