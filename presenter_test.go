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
	p.moduleDone("example.com/lib")
	p.mu.Lock()
	p.renderLocked(p.progress.started.Add(11 * time.Second))
	p.mu.Unlock()
	p.endProgress()
	p.status("OK", "Checked %d modules", 8)
	log := out.String()
	if strings.ContainsAny(log, "\r\x1b") || !strings.Contains(log, "Metadata 1/8 (12%)") || !strings.Contains(log, "[OK] Checked 8 modules") {
		t.Fatalf("unexpected plain progress: %q", log)
	}
	if strings.Contains(log, "example.com/lib") {
		t.Fatal("normal redirected output includes individual candidate details")
	}
}

func TestPresenterRetainsDiagnosticsAndRestoresScreenOnExit(t *testing.T) {
	var combined bytes.Buffer
	p := newPresenter(&combined, &combined, false)
	p.terminalSize = func() (int, int, bool) { return 100, 12, true }
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
	if !strings.Contains(log, "[WARN] Lookup failed\nFIRST\nsecond\nthird\nfourth\nLAST\n") || p.footerHeight != 0 {
		t.Fatalf("diagnostic or cleanup corrupted: %q", log)
	}
	if !strings.Contains(log, leaveView) {
		t.Fatal("presenter did not restore the terminal")
	}
}

func TestProgressFitsWidthAndKeepsVersion(t *testing.T) {
	now := time.Now()
	s := progressState{title: "Checking metadata", metadata: true, completed: 3, total: 20, started: now,
		item: "example.com/a/very/long/module/path/with/more/segments@v1.23.4", source: string(cacheShared)}
	for _, width := range []int{79, 99, 119} {
		lines := progressLines(s, 45, now.Add(3*time.Second), width)
		line := lines[1]
		if len([]rune(line)) > width || !strings.Contains(line, "@v1.23.4") || !strings.Contains(lines[0], "3/20 (15%)") || !strings.Contains(lines[0], "~45%") || len([]rune(lines[0])) > width || !strings.Contains(line, string(cacheShared)) {
			t.Errorf("bad progress width %d: %q", width, line)
		}
	}
	if strings.ContainsAny(shortenItem("example.com/control\npath@v1.0.0", 40), "\r\n\x1b") {
		t.Fatal("control character leaked into the live line")
	}
}

func TestTaskAfterFallbackResetsElapsedTime(t *testing.T) {
	var out bytes.Buffer
	p := newPresenter(&out, &out, false)
	p.beginMetadata(1, 1)
	p.endProgress()
	p.task("validating the next batch")
	line := progressLines(p.progress, 45, time.Now().Add(time.Second), 100)[1]
	if !strings.HasSuffix(line, " | Elapsed       1s") {
		t.Fatalf("stale task timer after metadata fallback: %q", line)
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
			p.moduleDone(fmt.Sprintf("example.com/lib%d", i))
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

func TestElapsedFieldStaysFixedWhileActionsChange(t *testing.T) {
	now := time.Now()
	for _, width := range []int{59, 79, 99} {
		s := progressState{title: "Dependencies", item: "example.com/a@v1.2.3", started: now}
		expected := -1
		for _, action := range []string{"updating dependencies", "go tidy", "go build", "running tests", "go vet"} {
			s.action = action
			line := progressLines(s, 50, now.Add(1900*time.Millisecond), width)[1]
			column := strings.Index(line, " | Elapsed")
			if expected < 0 {
				expected = column
			}
			if column != expected || len(line) != width || !strings.HasSuffix(line, "      1s") {
				t.Fatalf("timer moved for %q at width %d: %q", action, width, line)
			}
			earlier := progressLines(s, 50, now.Add(1100*time.Millisecond), width)[1]
			if line != earlier {
				t.Fatal("timer changed within the same second")
			}
		}
	}
}
