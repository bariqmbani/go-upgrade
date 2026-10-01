package main

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

// The presenter is the only owner of terminal output. Workers update its state;
// a timer paints that state without making dependency queries wait for a repaint.
type presenter struct {
	mu                sync.Mutex
	out, errOut       io.Writer
	verbose, color    bool
	terminalSize      func() (int, int, bool)
	errSharesTerminal bool
	work              workProgress
	pass              int
	active            map[string]activeLookup
	sequence          uint64
	footerHeight      int
	lastFrame         [2]string
	progress          progressState
	drawn             bool
	stop, done        chan struct{}
	closeOnce         sync.Once
}

type progressState struct {
	title, item, source string
	action              string
	started, lastLog    time.Time
	completed, total    int
	metadata            bool
}

func newPresenter(out, errOut io.Writer, verbose bool) *presenter {
	p := &presenter{out: out, errOut: errOut, verbose: verbose, work: newWorkProgress(false), errSharesTerminal: sameOutput(out, errOut)}
	file, ok := out.(*os.File)
	if ok && os.Getenv("TERM") != "" && os.Getenv("TERM") != "dumb" && os.Getenv("CI") == "" && term.IsTerminal(int(file.Fd())) {
		p.terminalSize = func() (int, int, bool) {
			width, height, err := term.GetSize(int(file.Fd()))
			return width, height, err == nil && width >= 60 && height >= 8
		}
		_, noColor := os.LookupEnv("NO_COLOR")
		_, _, usable := p.terminalSize()
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
		p.mu.Lock()
		p.releaseFooterLocked()
		p.mu.Unlock()
	})
}

type activeLookup struct {
	item, source string
	sequence     uint64
}

func sameOutput(out, errOut io.Writer) bool {
	a, b := reflect.ValueOf(out), reflect.ValueOf(errOut)
	if a.IsValid() && b.IsValid() && a.Comparable() && b.Comparable() && out == errOut {
		return true
	}
	if a, ok := out.(*os.File); ok {
		if b, ok := errOut.(*os.File); ok {
			aInfo, aErr := a.Stat()
			bInfo, bErr := b.Stat()
			return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
		}
	}
	return false
}

// The footer uses ordinary full-screen scrolling. No scrolling region or
// alternate screen is installed, so permanent output enters normal scrollback.
// The cursor rests on the blank margin row between redraws. Before a log block,
// erase the footer, append the block normally, then add two trailing newlines
// to leave three empty rows and restore the margin-row cursor.
func (p *presenter) clearLocked() {
	if p.footerHeight > 0 {
		for row := p.footerHeight - 2; row <= p.footerHeight; row++ {
			fmt.Fprintf(p.out, "\x1b[%d;1H\x1b[2K", row)
		}
		fmt.Fprintf(p.out, "\x1b[%d;1H", p.footerHeight-2)
	}
	p.drawn = false
}

func (p *presenter) releaseFooterLocked() {
	p.resizeLocked()
	p.clearLocked()
	p.footerHeight = 0
}

type outputTail struct {
	io.Writer
	last    byte
	written bool
}

func (w *outputTail) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if n > 0 {
		w.last, w.written = data[n-1], true
	}
	return n, err
}

func (p *presenter) writeLocked(w io.Writer, fn func(io.Writer) error) error {
	p.resizeLocked()
	p.clearLocked()
	terminal := p.footerHeight > 0 && (sameOutput(w, p.out) || p.errSharesTerminal && sameOutput(w, p.errOut))
	if !terminal {
		return fn(w)
	}
	tail := &outputTail{Writer: w}
	err := fn(tail)
	if tail.written {
		if tail.last != '\n' {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(p.out, "\n\n\x1b[%d;1H", p.footerHeight-2)
	}
	return err
}

func (p *presenter) text(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeLocked(p.out, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	})
}

func (p *presenter) detail(format string, args ...any) {
	if p.verbose {
		p.text(format, args...)
	}
}

func (p *presenter) status(status, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeLocked(p.out, func(w io.Writer) error {
		p.statusLocked(w, status, fmt.Sprintf(format, args...), p.color)
		return nil
	})
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
	p.writeLocked(p.errOut, func(w io.Writer) error {
		p.statusLocked(w, "WARN", title, false)
		if details != "" {
			fmt.Fprint(w, details)
			if !strings.HasSuffix(details, "\n") {
				fmt.Fprintln(w)
			}
		}
		return nil
	})
}

func (p *presenter) commandOutput(label, command string, log io.Reader, err error) error {
	if err == nil && !p.verbose {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.out
	if err != nil {
		w = p.errOut
	}
	return p.writeLocked(w, func(w io.Writer) error {
		if err != nil {
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
	})
}

func (p *presenter) fatal(step string, err error, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseFooterLocked()
	p.statusLocked(p.errOut, "FAIL", "Go upgrade failed", false)
	fmt.Fprintf(p.errOut, "  Step      : %s\n  Error     : %v\n  Exit code : %d\n", step, err, code)
}

func (p *presenter) configure(upgradeDeps bool) {
	p.mu.Lock()
	p.work = newWorkProgress(upgradeDeps)
	p.mu.Unlock()
}

func (p *presenter) beginPhase(stage progressStage, title string, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizeLocked()
	p.clearLocked()
	p.work.begin(stage, total)
	p.progress = progressState{}
	p.resumeLocked()
	if !p.interactiveLocked() {
		fmt.Fprintf(p.out, "\n%s...\n", title)
	}
}

func (p *presenter) resumeLocked() {
	if p.progress.title == "" {
		now := time.Now()
		started := p.work.started
		if started.IsZero() {
			started = now
		}
		p.progress = progressState{title: p.work.stage.label(), started: started, lastLog: now}
	}
}

func (p *presenter) task(action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumeLocked()
	p.progress.action, p.progress.source = action, ""
	if p.work.stage != stageDependencies {
		p.progress.item = action
	}
}

func (p *presenter) candidates(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumeLocked()
	p.progress.item = names[0]
	if len(names) > 1 {
		p.progress.item += fmt.Sprintf(" (+%d)", len(names)-1)
	}
}

func (p *presenter) checkDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.work.stage != stageDependencies && p.work.stage != stageDiscovery {
		p.work.advance()
	}
}

func (p *presenter) dependencyDone(module string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.work.resolve(module)
}

func (p *presenter) phaseDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.work.finish()
}

func (p *presenter) phaseCounts() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%d/%d (%d%%)", p.work.completed, p.work.total, phasePercent(p.work.completed, p.work.total))
}

func (p *presenter) success() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.work.overall = 100
	p.progress = progressState{}
	p.releaseFooterLocked()
}

func (p *presenter) beginMetadata(total, workers int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizeLocked()
	p.clearLocked()
	p.work.begin(stageDiscovery, total)
	p.progress = progressState{}
	p.resumeLocked()
	p.progress.metadata = true
	p.pass++
	if p.pass > 1 {
		p.progress.title += fmt.Sprintf(" #%d", p.pass)
	}
	p.active = make(map[string]activeLookup)
	p.writeLocked(p.out, func(w io.Writer) error {
		fmt.Fprintf(w, "\nChecking candidates: %s, %s\n", humanCount(total, "module"), humanCount(workers, "worker"))
		return nil
	})
}

func (p *presenter) lookup(event lookupEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.progress.metadata {
		source := string(event.source)
		if event.kind == "versions" && (event.source == cacheMiss || event.source == cacheRefresh) {
			source = "Finding versions (" + source + ")"
		}
		p.sequence++
		module, _, _ := strings.Cut(event.item, "@")
		p.active[module] = activeLookup{event.item, source, p.sequence}
		p.selectActiveLocked()
	} else if p.work.stage == stageDependencies {
		p.resumeLocked()
		p.progress.item, p.progress.source = event.item, string(event.source)
	}
	if p.verbose {
		p.writeLocked(p.out, func(w io.Writer) error {
			kind := "Candidate"
			if event.kind == "versions" {
				kind = "Version list"
			}
			fmt.Fprintf(w, "  %s %s: %s\n", kind, event.item, event.source)
			return nil
		})
	}
}

func (p *presenter) selectActiveLocked() {
	var latest activeLookup
	for _, lookup := range p.active {
		if lookup.sequence > latest.sequence {
			latest = lookup
		}
	}
	p.progress.item, p.progress.source = latest.item, latest.source
}

func (p *presenter) moduleDone(module string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, module)
	p.selectActiveLocked()
	p.work.resolve(module)
}

func (p *presenter) endProgress() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizeLocked()
	p.clearLocked()
	p.progress = progressState{}
}

func (p *presenter) interactiveLocked() bool {
	if p.terminalSize == nil {
		return false
	}
	_, _, ok := p.terminalSize()
	return ok
}

func (p *presenter) resizeLocked() (int, int, bool) {
	if p.terminalSize == nil {
		return 0, 0, false
	}
	width, height, ok := p.terminalSize()
	if p.footerHeight > 0 && (!ok || height != p.footerHeight) {
		// On shrink, coordinates outside the new screen would clamp to the
		// bottom row. Only erase old footer rows still inside the screen.
		for row := p.footerHeight - 2; row <= min(p.footerHeight, height); row++ {
			if row > 0 {
				fmt.Fprintf(p.out, "\x1b[%d;1H\x1b[2K", row)
			}
		}
		p.footerHeight, p.drawn = 0, false
	}
	return width, height, ok
}

func (p *presenter) snapshotLocked() progressState {
	s := p.progress
	s.completed, s.total = p.work.completed, p.work.total
	return s
}

func (p *presenter) renderLocked(now time.Time) {
	if p.progress.title == "" {
		return
	}
	s := p.snapshotLocked()
	width, height, interactive := p.resizeLocked()
	if !interactive {
		if now.Sub(s.lastLog) >= 10*time.Second {
			fmt.Fprintf(p.out, "  Overall ~%d%% | %s %d/%d (%d%%) | %s elapsed\n", p.work.overall, s.title, s.completed, s.total, phasePercent(s.completed, s.total), humanDuration(now.Sub(s.started)))
			p.progress.lastLog = now
		}
		return
	}
	lines := progressLines(s, p.work.overall, now, width-1)
	if p.drawn && p.lastFrame == lines {
		return
	}
	if p.footerHeight == 0 {
		// Reserve space using normal scrolling, without erasing older logs.
		fmt.Fprint(p.out, "\n\n\n")
		p.footerHeight = height
	}
	p.clearLocked()
	fmt.Fprintf(p.out, "\x1b[%d;1H%s\x1b[%d;1H%s\x1b[%d;1H", height-1, lines[0], height, lines[1], height-2)
	p.drawn, p.lastFrame = true, lines
}

func progressLines(s progressState, overall int, now time.Time, width int) [2]string {
	counts := fmt.Sprintf("%s %d/%d (%d%%)", s.title, s.completed, s.total, phasePercent(s.completed, s.total))
	// Compact labels leave space for both percentages even on a 60-column TTY.
	counts = strings.ReplaceAll(counts, "Final checks", "Final")
	counts = strings.ReplaceAll(counts, "Dependencies", "Deps")
	barWidth := min(20, max(4, width-len([]rune(counts))-len("Overall [] ~100% | ")))
	filled := min(barWidth, max(0, overall)*barWidth/100)
	bar := "[" + strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled) + "]"
	first := fmt.Sprintf("Overall %s ~%d%% | %s", bar, overall, counts)
	suffix := " | " + humanDuration(now.Sub(s.started))
	if s.source != "" {
		suffix = " | " + s.source + suffix
	} else if s.item != "" && s.action != "" && s.item != s.action {
		suffix = " | " + shortAction(s.action) + suffix
	}
	item := s.item
	if item == "" {
		item = "Preparing checks"
		if s.total > 0 && s.completed == s.total {
			item = "Checks complete"
		}
	}
	second := shortenItem(item, max(0, width-len([]rune(suffix)))) + suffix
	return [2]string{shorten(first, width), shorten(second, width)}
}

func shortAction(action string) string {
	switch {
	case strings.Contains(action, "race"):
		return "Race tests"
	case strings.Contains(action, "build"):
		return "Build"
	case strings.Contains(action, "tests"):
		return "Tests"
	case strings.Contains(action, "tidy"):
		return "Tidy"
	case strings.Contains(action, "vet"):
		return "Vet"
	case strings.Contains(action, "updating"):
		return "Updating"
	default:
		return action
	}
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
