package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPresenterRedirectedProgress(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newPresenter(&out, &errOut, false)
	p.beginMetadata(8, 4)
	p.lookup(lookupEvent{"metadata", "example.com/lib@v1.2.0", cacheShared})
	p.moduleDone()
	p.mu.Lock()
	p.renderLocked(p.progress.started.Add(11 * time.Second))
	p.mu.Unlock()
	p.endProgress()
	p.status("OK", "Checked %d modules", 8)
	log := out.String()
	if strings.ContainsAny(log, "\r\x1b") || !strings.Contains(log, "1/8 modules") || !strings.Contains(log, "[OK] Checked 8 modules") {
		t.Fatalf("unexpected plain progress: %q", log)
	}
	if strings.Contains(log, "example.com/lib") {
		t.Fatal("normal redirected output includes individual candidate details")
	}
}

func TestPresenterClearsBeforeDiagnosticsAndExit(t *testing.T) {
	var combined bytes.Buffer
	p := newPresenter(&combined, &combined, false)
	p.terminalWidth = func() (int, bool) { return 100, true }
	p.beginMetadata(2, 2)
	p.lookup(lookupEvent{"metadata", "example.com/lib@v1.2.0", cacheMiss})
	p.mu.Lock()
	p.renderLocked(time.Now())
	p.mu.Unlock()
	p.diagnostic("Lookup failed", "FIRST\nsecond\nthird\nfourth\nLAST\n")
	p.mu.Lock()
	p.renderLocked(time.Now())
	p.mu.Unlock()
	p.close()
	log := combined.String()
	if !strings.Contains(log, "\r\x1b[2K[WARN] Lookup failed\nFIRST\nsecond\nthird\nfourth\nLAST\n") || !strings.HasSuffix(log, "\r\x1b[2K") {
		t.Fatalf("diagnostic or cleanup corrupted: %q", log)
	}
	if strings.Contains(log, "\x1b[?25") {
		t.Fatal("presenter changes cursor visibility")
	}
}

func TestProgressFitsWidthAndKeepsVersion(t *testing.T) {
	now := time.Now()
	s := progressState{title: "Checking metadata", metadata: true, completed: 3, total: 20, started: now,
		item: "example.com/a/very/long/module/path/with/more/segments@v1.23.4", source: string(cacheShared)}
	for _, width := range []int{79, 99, 119} {
		line := progressLine(s, now.Add(3*time.Second), width)
		if len([]rune(line)) > width || !strings.Contains(line, "@v1.23.4") || !strings.Contains(line, "3/20") || !strings.Contains(line, string(cacheShared)) {
			t.Errorf("bad progress width %d: %q", width, line)
		}
	}
	if strings.ContainsAny(shortenItem("example.com/control\npath@v1.0.0", 40), "\r\n\x1b") {
		t.Fatal("control character leaked into the live line")
	}
}

func TestPresenterConcurrentWorkers(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newPresenter(&out, &errOut, true)
	p.beginMetadata(16, 4)
	p.start()
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			p.lookup(lookupEvent{"metadata", fmt.Sprintf("example.com/lib%d@v1.1.0", i), cacheShared})
			p.diagnostic(fmt.Sprintf("worker %d", i), fmt.Sprintf("BEGIN-%d\nEND-%d\n", i, i))
			p.moduleDone()
		})
	}
	workers.Wait()
	p.close()
	if p.progress.title != "" || strings.Count(out.String(), "Cached (shared)") != 16 {
		t.Fatalf("lost progress events: %s", out.String())
	}
	for i := range 16 {
		if !strings.Contains(errOut.String(), fmt.Sprintf("BEGIN-%d\nEND-%d\n", i, i)) {
			t.Fatalf("interleaved diagnostic for worker %d", i)
		}
	}
}

func TestPresenterAlwaysShowsCompleteCommandFailures(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var out, errOut bytes.Buffer
		p := newPresenter(&out, &errOut, verbose)
		transcript := "FIRST\nsecond\nthird\nfourth\nLAST\n"
		err := &commandError{command: "make clean build", code: 42, err: errors.New("exit status 42")}
		if copyErr := p.commandOutput("Candidate build", err.command, strings.NewReader(transcript), err); copyErr != nil {
			t.Fatal(copyErr)
		}
		if !strings.Contains(errOut.String(), transcript) || !strings.Contains(errOut.String(), "Exit code : 42") {
			t.Fatalf("missing failure details (verbose=%t): %s", verbose, errOut.String())
		}
		if copyErr := p.commandOutput("Successful build", "go build ./...", strings.NewReader("SUCCESS-OUTPUT"), nil); copyErr != nil {
			t.Fatal(copyErr)
		}
		if strings.Contains(out.String(), "SUCCESS-OUTPUT") != verbose {
			t.Fatalf("wrong successful transcript visibility: %q", out.String())
		}
	}
}
