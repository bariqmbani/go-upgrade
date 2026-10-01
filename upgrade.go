package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type request struct {
	module, version string
}

func (r request) String() string { return r.module + "@" + r.version }

func (a *app) makeLookups() error {
	// Metadata queries never share the project's mutable go.mod/go.sum.
	// A neutral module also keeps shared metadata independent of project replaces.
	for i := 0; i < a.opts.workers; i++ {
		dir := filepath.Join(a.work, fmt.Sprintf("lookup-%d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		data := "module upgrade-go.invalid/lookup\n\ngo " + a.opts.target + "\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(data), 0o600); err != nil {
			return err
		}
		a.lookupDirs = append(a.lookupDirs, dir)
	}
	return nil
}

func (a *app) collectBatch() ([]request, error) {
	modules, err := a.listModules()
	if err != nil {
		return nil, err
	}
	var pending []listedModule
	for _, m := range modules {
		if m.Replace == nil && a.accepted.required[m.Path] && !a.examined[m.Path] {
			a.examined[m.Path] = true
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	workers := min(a.opts.workers, len(pending))
	fmt.Fprintf(a.out, "==> Selecting newest candidates compatible with Go %s...\n    Checking %d modules with %d metadata workers.\n", a.opts.target, len(pending), workers)
	type discovery struct {
		request request
		log     string
	}
	results := make([]discovery, len(pending))
	jobs := make(chan int, len(pending))
	for i := range pending {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var finished atomic.Int64
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			for i := range jobs {
				if a.ctx.Err() != nil {
					return
				}
				m := pending[i]
				catalog := a.cache.catalog(dir, m.Path)
				var log strings.Builder
				log.WriteString(catalog.log)
				for _, v := range catalog.candidates {
					if a.ctx.Err() != nil {
						return
					}
					if !newer(v, m.Version) || (strings.Contains(v, "-") && v != catalog.latest) {
						continue
					}
					metadata := a.cache.supports(dir, m.Path, v)
					if metadata.status == 0 {
						results[i].request = request{m.Path, v}
						log.WriteString(metadata.log)
						break
					}
					if metadata.status == 1 {
						fmt.Fprintf(&log, "    Skipping %s@%s: incompatible module metadata.\n", m.Path, v)
					} else {
						fmt.Fprintf(&log, "    Skipping %s@%s: metadata lookup failed.\n", m.Path, v)
					}
					log.WriteString(tail(metadata.log, 2))
				}
				results[i].log = log.String()
				finished.Add(1)
			}
		}(a.lookupDirs[worker])
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	waiting := true
	for waiting {
		select {
		case <-done:
			waiting = false
		case <-ticker.C:
			fmt.Fprintf(a.out, "    Metadata progress: %d/%d modules checked.\n", finished.Load(), len(pending))
		}
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	var batch []request
	// Keep log and upgrade order deterministic regardless of worker completion order.
	for _, result := range results {
		fmt.Fprint(a.out, result.log)
		if result.request.module != "" {
			batch = append(batch, result.request)
		}
	}
	return batch, nil
}

func (a *app) checkVersions(requests []request) error {
	modules, err := a.listModules()
	if err != nil {
		return err
	}
	after := make(map[string]string, len(modules))
	for _, m := range modules {
		after[m.Path] = m.Version
	}
	for path, before := range a.accepted.modules {
		if a.accepted.required[path] && after[path] != "" && newer(before, after[path]) {
			return fmt.Errorf("rejected downgrade of %s from %s to %s", path, before, after[path])
		}
	}
	for _, r := range requests {
		if after[r.module] == "" || newer(r.version, after[r.module]) {
			return fmt.Errorf("requested %s was not retained after target-constrained tidy", r)
		}
	}
	return nil
}

func (a *app) tryUpgrade(requests []request) (bool, string, error) {
	var log bytes.Buffer
	args := []string{"get"}
	for _, r := range requests {
		args = append(args, r.String())
	}
	args = append(args, "go@"+a.opts.target, "toolchain@none")
	err := a.runner.goRun(a.dir, &log, args...)
	if err == nil {
		err = a.tidy(&log)
	}
	if err == nil {
		err = a.checkVersions(requests)
	}
	if err == nil {
		err = a.validate(&log)
	}
	if err == nil {
		return true, log.String(), nil
	}
	fmt.Fprintln(&log, err)
	if restoreErr := a.accepted.restore(a.dir); restoreErr != nil {
		return false, log.String(), fmt.Errorf("restoring last validated graph: %w", restoreErr)
	}
	if a.ctx.Err() != nil {
		return false, log.String(), a.ctx.Err()
	}
	return false, log.String(), nil
}

func (a *app) fallback(r request) error {
	previous := a.accepted.modules[r.module]
	catalog := a.cache.catalog(a.lookupDirs[0], r.module)
	for _, v := range catalog.candidates {
		if !newer(r.version, v) || !newer(v, previous) || (strings.Contains(v, "-") && v != catalog.latest) {
			continue
		}
		if err := a.ctx.Err(); err != nil {
			return err
		}
		if a.cache.supports(a.lookupDirs[0], r.module, v).status != 0 {
			continue
		}
		candidate := request{r.module, v}
		fmt.Fprintf(a.out, "==> Trying earlier version %s...\n", candidate)
		ok, log, err := a.tryUpgrade([]request{candidate})
		if err != nil {
			return err
		}
		if ok {
			if err := a.acceptGraph(); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "    Accepted %s\n", candidate)
			return nil
		}
		fmt.Fprintf(a.out, "    Rejected %s; restored the last validated graph.\n%s", candidate, tail(log, 3))
	}
	fmt.Fprintf(a.out, "    Retained %s@%s: no newer candidate passed validation.\n", r.module, previous)
	return nil
}

func family(module string) string {
	parts := strings.Split(module, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/")
	}
	return module
}

func batchSplit(batch []request) int {
	count := len(batch)
	midpoint := count / 2
	for distance := 0; distance < count; distance++ {
		for _, boundary := range []int{midpoint - distance, midpoint + distance} {
			if boundary > 0 && boundary < count && family(batch[boundary-1].module) != family(batch[boundary].module) {
				return boundary
			}
		}
	}
	return midpoint
}

func (a *app) upgradeBatch(input []request) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	var batch []request
	for _, r := range input {
		previous := a.accepted.modules[r.module]
		if a.accepted.required[r.module] && previous != "" && newer(r.version, previous) {
			batch = append(batch, r)
		}
	}
	if len(batch) == 0 {
		return nil
	}
	fmt.Fprintf(a.out, "==> Trying dependency batch (%d modules)...\n", len(batch))
	ok, log, err := a.tryUpgrade(batch)
	if err != nil {
		return err
	}
	if ok {
		if err := a.acceptGraph(); err != nil {
			return err
		}
		requests := make([]string, len(batch))
		for i, r := range batch {
			requests[i] = r.String()
		}
		fmt.Fprintf(a.out, "    Accepted batch: %s\n", strings.Join(requests, " "))
		return nil
	}
	fmt.Fprintf(a.out, "    Batch failed; restored the last validated graph.\n%s", tail(log, 3))
	if len(batch) == 1 {
		return a.fallback(batch[0])
	}
	split := batchSplit(batch)
	fmt.Fprintf(a.out, "    Splitting into %d and %d modules.\n", split, len(batch)-split)
	if err := a.upgradeBatch(batch[:split]); err != nil {
		return err
	}
	return a.upgradeBatch(batch[split:])
}
