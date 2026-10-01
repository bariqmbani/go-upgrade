package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

// The presenter is the only owner of terminal output. Workers update its state;
// a timer paints that state without making dependency queries wait for a repaint.
type presenter struct {
	mu             sync.Mutex
	out, errOut    io.Writer
	verbose, color bool
	terminalWidth  func() (int, bool)
	progress       progressState
	drawn          bool
	stop, done     chan struct{}
	closeOnce      sync.Once
}

type progressState struct {
	title, item, source string
	started, lastLog    time.Time
	completed, total    int
	metadata            bool
}

func newPresenter(out, errOut io.Writer, verbose bool) *presenter {
	p := &presenter{out: out, errOut: errOut, verbose: verbose}
	file, ok := out.(*os.File)
	if ok && os.Getenv("TERM") != "" && os.Getenv("TERM") != "dumb" && os.Getenv("CI") == "" && term.IsTerminal(int(file.Fd())) {
		p.terminalWidth = func() (int, bool) {
			width, _, err := term.GetSize(int(file.Fd()))
			return width, err == nil && width >= 60
		}
		_, noColor := os.LookupEnv("NO_COLOR")
		_, usable := p.terminalWidth()
		p.color = usable && !noColor
	}
	return p
}

func (p *presenter) start() {
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				p.mu.Lock()
				p.renderLocked(now)
				p.mu.Unlock()
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *presenter) close() {
	p.closeOnce.Do(func() {
		if p.stop != nil {
			close(p.stop)
			<-p.done
		}
		p.endProgress()
	})
}

func (p *presenter) clearLocked() {
	if p.drawn {
		fmt.Fprint(p.out, "\r\x1b[2K")
		p.drawn = false
	}
}

func (p *presenter) text(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	fmt.Fprintf(p.out, format, args...)
}

func (p *presenter) detail(format string, args ...any) {
	if p.verbose {
		p.text(format, args...)
	}
}

func (p *presenter) status(status, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.statusLocked(p.out, status, fmt.Sprintf(format, args...), p.color)
}

func (p *presenter) statusLocked(w io.Writer, status, message string, colorEnabled bool) {
	label := "[" + status + "]"
	if colorEnabled {
		color := map[string]string{"OK": "32", "SKIP": "36", "WARN": "33", "FAIL": "31"}[status]
		if color != "" {
			label = "\x1b[" + color + "m" + label + "\x1b[0m"
		}
	}
	fmt.Fprintf(w, "%s %s\n", label, message)
}

// Diagnostics are permanent and never depend on --verbose. Hold the output lock
// for the whole block so concurrent workers cannot interleave their errors.
func (p *presenter) diagnostic(title, details string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.statusLocked(p.errOut, "WARN", title, false)
	if details != "" {
		fmt.Fprint(p.errOut, details)
		if !strings.HasSuffix(details, "\n") {
			fmt.Fprintln(p.errOut)
		}
	}
}

func (p *presenter) commandOutput(label, command string, log io.Reader, err error) error {
	if err == nil && !p.verbose {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	w := p.out
	if err != nil {
		w = p.errOut
		p.statusLocked(w, "FAIL", label, false)
		fmt.Fprintf(w, "  Command   : %s\n  Exit code : %d\n  Error     : %v\n", command, errorCode(err), err)
	} else {
		fmt.Fprintf(w, "  Command: %s\n", command)
	}
	n, copyErr := io.Copy(w, log)
	if n > 0 {
		fmt.Fprintln(w)
	}
	return copyErr
}

func (p *presenter) fatal(step string, err error, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.statusLocked(p.errOut, "FAIL", "Go upgrade failed", false)
	fmt.Fprintf(p.errOut, "  Step      : %s\n  Error     : %v\n  Exit code : %d\n", step, err, code)
}

func (p *presenter) beginTask(title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.progress = progressState{title: title, started: time.Now(), lastLog: time.Now()}
	if !p.interactiveLocked() {
		fmt.Fprintf(p.out, "\n%s...\n", title)
	}
}

func (p *presenter) task(title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.progress.metadata {
		if p.progress.started.IsZero() {
			now := time.Now()
			p.progress.started, p.progress.lastLog = now, now
		}
		p.progress.title = title
	}
}

func (p *presenter) beginMetadata(total, workers int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	now := time.Now()
	p.progress = progressState{title: "Checking metadata", started: now, lastLog: now, total: total, metadata: true}
	fmt.Fprintf(p.out, "\nChecking candidates: %s, %s\n", humanCount(total, "module"), humanCount(workers, "worker"))
}

func (p *presenter) lookup(event lookupEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.progress.metadata {
		p.progress.item, p.progress.source = event.item, string(event.source)
		if event.kind == "versions" && (event.source == cacheMiss || event.source == cacheRefresh) {
			p.progress.source = "Finding versions (" + string(event.source) + ")"
		}
	}
	if p.verbose {
		p.clearLocked()
		kind := "Candidate"
		if event.kind == "versions" {
			kind = "Version list"
		}
		fmt.Fprintf(p.out, "  %s %s: %s\n", kind, event.item, event.source)
	}
}

func (p *presenter) moduleDone() {
	p.mu.Lock()
	p.progress.completed++
	p.mu.Unlock()
}

func (p *presenter) endProgress() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.progress = progressState{}
}

func (p *presenter) interactiveLocked() bool {
	if p.terminalWidth == nil {
		return false
	}
	_, ok := p.terminalWidth()
	return ok
}

func (p *presenter) renderLocked(now time.Time) {
	s := p.progress
	if s.title == "" {
		return
	}
	width, interactive := 0, false
	if p.terminalWidth != nil {
		width, interactive = p.terminalWidth()
	}
	if !interactive {
		p.clearLocked()
		if now.Sub(s.lastLog) >= 10*time.Second {
			line := s.title
			if s.metadata {
				line += fmt.Sprintf(" %d/%d modules", s.completed, s.total)
			}
			fmt.Fprintf(p.out, "  %s (%s elapsed)\n", line, humanDuration(now.Sub(s.started)))
			p.progress.lastLog = now
		}
		return
	}
	line := progressLine(s, now, width-1)
	p.clearLocked()
	fmt.Fprintf(p.out, "\r%s", line)
	p.drawn = true
}

func progressLine(s progressState, now time.Time, width int) string {
	prefix := s.title
	if s.metadata {
		prefix += fmt.Sprintf(" %d/%d", s.completed, s.total)
	}
	suffix := " | " + humanDuration(now.Sub(s.started))
	if s.source != "" {
		suffix = " | " + s.source + suffix
	}
	if s.item != "" {
		available := width - len([]rune(prefix+suffix)) - 3
		if available >= 8 {
			return prefix + " | " + shortenItem(s.item, available) + suffix
		}
	}
	return shorten(prefix+suffix, width)
}

func shortenItem(item string, width int) string {
	item = safeLine(item)
	if len([]rune(item)) <= width {
		return item
	}
	if path, version, ok := strings.Cut(item, "@"); ok && width-len(version)-1 >= 4 {
		return shorten(path, width-len(version)-1) + "@" + version
	}
	return shorten(item, width)
}

func safeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func shorten(s string, width int) string {
	runes := []rune(safeLine(s))
	if width <= 0 {
		return ""
	}
	if len(runes) <= width {
		return string(runes)
	}
	if width < 4 {
		return string(runes[:width])
	}
	left := (width - 3) / 2
	return string(runes[:left]) + "..." + string(runes[len(runes)-(width-3-left):])
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func humanCount(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}
