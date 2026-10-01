package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const enterView = "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h"
const leaveView = "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"

type terminalInput struct {
	file       *os.File
	state      *term.State
	stop, done chan struct{}
	events     chan string
}

func startTerminalInput(file *os.File) (*terminalInput, error) {
	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	in := &terminalInput{file: file, state: state, stop: make(chan struct{}), done: make(chan struct{}), events: make(chan string, 64)}
	go in.read()
	return in, nil
}

func (in *terminalInput) close() {
	close(in.stop)
	<-in.done
	term.Restore(int(in.file.Fd()), in.state)
}

func (in *terminalInput) read() {
	defer close(in.done)
	defer func() {
		select {
		case in.events <- "inputclosed":
		case <-in.stop:
		}
	}()
	var pending string
	var buf [256]byte
	fds := []unix.PollFd{{Fd: int32(in.file.Fd()), Events: unix.POLLIN}}
	for {
		select {
		case <-in.stop:
			return
		default:
		}
		n, err := unix.Poll(fds, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return
		}
		if n == 0 {
			continue
		}
		select {
		case <-in.stop:
			return
		default:
		}
		n, err = unix.Read(int(in.file.Fd()), buf[:])
		if err != nil || n == 0 {
			return
		}
		pending += string(buf[:n])
		var events []string
		pending, events = inputEvents(pending)
		for _, event := range events {
			select {
			case in.events <- event:
			case <-in.stop:
				return
			}
		}
	}
}

// Parse complete input sequences and retain a bounded incomplete suffix. Unknown
// keys are ignored, so keyboard input never leaks into the restored shell.
func inputEvents(input string) (string, []string) {
	var events []string
	for len(input) > 0 {
		if input[0] == 3 {
			events = append(events, "interrupt")
			input = input[1:]
			continue
		}
		if input[0] == 'G' {
			events = append(events, "end")
		}
		if input[0] != '\x1b' {
			input = input[1:]
			continue
		}
		if len(input) == 1 {
			break
		}
		if input[1] != '[' && input[1] != 'O' {
			input = input[1:]
			continue
		}
		end := 2
		for end < len(input) && !(input[end] >= 0x40 && input[end] <= 0x7e) {
			end++
		}
		if end == len(input) {
			if strings.ContainsRune(input, 3) {
				events = append(events, "interrupt")
				input = ""
			}
			if len(input) > 64 {
				input = ""
			}
			break
		}
		seq := input[:end+1]
		input = input[end+1:]
		event := map[string]string{
			"\x1b[A": "up", "\x1b[B": "down", "\x1bOA": "up", "\x1bOB": "down",
			"\x1b[5~": "pageup", "\x1b[6~": "pagedown", "\x1b[H": "home", "\x1b[F": "end",
			"\x1bOH": "home", "\x1bOF": "end", "\x1b[1~": "home", "\x1b[4~": "end",
			"\x1b[7~": "home", "\x1b[8~": "end",
		}[seq]
		if strings.HasPrefix(seq, "\x1b[<") && strings.HasSuffix(seq, "M") {
			var button, x, y int
			if n, _ := fmt.Sscanf(seq, "\x1b[<%d;%d;%dM", &button, &x, &y); n == 3 {
				switch button & 0xc3 {
				case 0:
					if button == 0 {
						event = fmt.Sprintf("click:%d:%d", x, y)
					}
				case 64:
					event = "wheelup"
				case 65:
					event = "wheeldown"
				}
			}
		}
		if event != "" {
			events = append(events, event)
		}
	}
	return input, events
}

func (p *presenter) inputLocked(event string) {
	if event == "inputclosed" {
		p.releaseFooterLocked()
		return
	}
	if event == "interrupt" {
		if p.interrupt != nil {
			p.interrupt()
		}
		return
	}
	if p.view == nil {
		return
	}
	if coordinates, ok := strings.CutPrefix(event, "click:"); ok {
		var x, y int
		if n, _ := fmt.Sscanf(coordinates, "%d:%d", &x, &y); n == 2 {
			p.view.clickBottom(x, y)
		}
		return
	}
	switch event {
	case "up":
		p.view.scroll(-1)
	case "down":
		p.view.scroll(1)
	case "wheelup":
		p.view.scroll(-3)
	case "wheeldown":
		p.view.scroll(3)
	case "pageup":
		p.view.scroll(-(p.view.height - 3))
	case "pagedown":
		p.view.scroll(p.view.height - 3)
	case "home":
		p.view.top, p.view.follow = 0, false
	case "end":
		p.view.follow = true
	}
}

func (p *presenter) ensureViewLocked() bool {
	if p.plain || p.terminalSize == nil {
		return false
	}
	width, height, ok := p.terminalSize()
	if !ok {
		p.releaseFooterLocked()
		return false
	}
	if p.view != nil {
		if err := p.view.resize(width, height); err != nil {
			p.releaseFooterLocked()
			return false
		}
		p.footerHeight = height
		return true
	}
	v, err := newTerminalView(width, height)
	if err != nil {
		p.plain = true
		return false
	}
	if p.inputFile != nil {
		p.input, err = startTerminalInput(p.inputFile)
		if err != nil {
			v.dispose()
			p.plain = true
			return false
		}
	}
	v.color = p.color
	p.view, p.footerHeight = v, height
	if _, err := io.WriteString(p.out, enterView); err != nil {
		p.releaseFooterLocked()
		return false
	}
	return true
}

func (p *presenter) releaseFooterLocked() {
	p.plain = true
	if p.input != nil {
		p.input.close()
		p.input = nil
	}
	if p.view != nil {
		io.WriteString(p.out, leaveView)
		if err := p.view.replay(p.out, p.errOut); err != nil {
			fmt.Fprintf(p.errOut, "[WARN] Could not replay terminal transcript: %v\n", err)
		}
		p.view.dispose()
		p.view = nil
	}
	p.footerHeight = 0
}

// Each block is ordered by outputMu, while individual writes briefly acquire mu.
// The renderer can paint between chunks of even very large command transcripts.
type presenterOutput struct {
	p   *presenter
	out io.Writer
}

func (w presenterOutput) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		chunk := data[:min(len(data), 32*1024)]
		w.p.mu.Lock()
		n, err := w.p.writeChunkLocked(w.out, chunk)
		w.p.mu.Unlock()
		written += n
		if err != nil {
			return written, err
		}
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
		data = data[n:]
	}
	return written, nil
}

func (p *presenter) writeChunkLocked(out io.Writer, data []byte) (int, error) {
	terminal := sameOutput(out, p.out) || p.errSharesTerminal && sameOutput(out, p.errOut)
	if !terminal {
		return out.Write(data)
	}
	if !p.ensureViewLocked() {
		return out.Write(data)
	}
	if err := p.view.capture(data, sameWriter(out, p.errOut) && !sameWriter(p.out, p.errOut)); err != nil {
		p.releaseFooterLocked()
		return out.Write(data)
	}
	if err := p.view.appendDisplay(data); err != nil {
		// The complete raw record is already captured, so fallback replays it once.
		p.releaseFooterLocked()
	}
	return len(data), nil
}

func (p *presenter) renderLocked(now time.Time) {
	if p.ensureViewLocked() {
		if p.progress.title != "" {
			p.lastFrame = progressLines(p.snapshotLocked(), p.work.overall, now, p.view.width)
		}
		if err := p.view.paint(p.out, p.lastFrame); err != nil {
			p.releaseFooterLocked()
		}
		return
	}
	s := p.snapshotLocked()
	if s.title != "" && now.Sub(s.lastLog) >= 10*time.Second && p.outputMu.TryLock() {
		fmt.Fprintf(p.out, "  Overall ~%d%% | %s %d/%d (%d%%) | %s elapsed\n", p.work.overall, s.title, s.completed, s.total, phasePercent(s.completed, s.total), humanDuration(now.Sub(s.started)))
		p.progress.lastLog = now
		p.outputMu.Unlock()
	}
}
