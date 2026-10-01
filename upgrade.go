package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	a.ui.beginMetadata(len(pending), workers)
	defer a.ui.endProgress()
	type discovery struct {
		request          request
		excluded, failed int
	}
	results := make([]discovery, len(pending))
	jobs := make(chan int, len(pending))
	for i := range pending {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			dir := a.lookupDirs[worker]
			for i := range jobs {
				if a.ctx.Err() != nil {
					return
				}
				m := pending[i]
				catalog := a.cache.catalog(dir, m.Path)
				a.reportCatalog(m.Path, catalog)
				for _, v := range catalog.candidates {
					if a.ctx.Err() != nil {
						return
					}
					if !newer(v, m.Version) || (strings.Contains(v, "-") && v != catalog.latest) {
						continue
					}
					entry := a.cache.supports(dir, m.Path, v)
					a.reportMetadata(request{m.Path, v}, entry)
					if entry.status == 0 {
						results[i].request = request{m.Path, v}
						break
					}
					if entry.status == 1 {
						results[i].excluded++
					} else {
						results[i].failed++
					}
				}
				a.ui.moduleDone()
			}
		})
	}
	wg.Wait()
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	var batch []request
	excluded, failed := 0, 0
	// Progress follows execution; upgrade order remains deterministic.
	for _, result := range results {
		excluded += result.excluded
		failed += result.failed
		if result.request.module != "" {
			batch = append(batch, result.request)
		}
	}
	a.ui.endProgress()
	a.ui.text("  Checked %d modules; selected %d upgrades\n", len(pending), len(batch))
	if excluded > 0 {
		a.ui.text("  Skipped %d candidates incompatible with Go %s or the module path\n", excluded, a.opts.target)
	}
	if failed > 0 {
		a.ui.status("WARN", "%d candidate metadata lookups failed; see diagnostics", failed)
	}
	return batch, nil
}

func (a *app) reportCatalog(module string, entry catalog) {
	if entry.log != "" {
		a.ui.diagnostic("Version lookup or cache warning: "+module, entry.log)
	}
}

func (a *app) reportMetadata(candidate request, entry metadata) {
	if entry.warning != "" {
		a.ui.diagnostic("Metadata cache warning: "+candidate.String(), entry.warning)
	}
	switch entry.status {
	case 0:
		a.ui.detail("  Compatible: %s (%s)\n", candidate, entry.source)
	case 1:
		a.ui.detail("  Skipped: %s (%s)\n    %s\n", candidate, entry.source, strings.TrimSpace(entry.log))
	default:
		a.ui.diagnostic("Metadata lookup failed: "+candidate.String()+" ("+string(entry.source)+")", entry.log)
	}
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

func (a *app) tryUpgrade(requests []request) (bool, error) {
	args := []string{"get"}
	names := make([]string, len(requests))
	for i, r := range requests {
		args = append(args, r.String())
		names[i] = r.String()
	}
	a.candidateContext = strings.Join(names, " ")
	defer func() { a.candidateContext = "" }()
	args = append(args, "go@"+a.opts.target, "toolchain@none")
	err := a.runGo("updating candidate dependencies", args...)
	if err == nil {
		err = a.tidy("candidate module tidy")
	}
	if err == nil {
		a.step = "verifying candidate versions"
		err = a.checkVersions(requests)
	}
	if err == nil {
		err = a.validate()
	}
	if err == nil {
		return true, nil
	}
	a.ui.diagnostic("Candidate validation failed", fmt.Sprintf("  Candidates: %s\n  Step: %s\n  Error: %v\n", a.candidateContext, a.step, err))
	if restoreErr := a.accepted.restore(a.dir); restoreErr != nil {
		return false, fmt.Errorf("restoring last validated graph: %w", restoreErr)
	}
	if a.ctx.Err() != nil {
		return false, a.ctx.Err()
	}
	return false, nil
}

func (a *app) fallback(r request) error {
	previous := a.accepted.modules[r.module]
	a.ui.beginMetadata(1, 1)
	defer a.ui.endProgress()
	catalog := a.cache.catalog(a.lookupDirs[0], r.module)
	a.reportCatalog(r.module, catalog)
	for _, v := range catalog.candidates {
		if !newer(r.version, v) || !newer(v, previous) || (strings.Contains(v, "-") && v != catalog.latest) {
			continue
		}
		if err := a.ctx.Err(); err != nil {
			return err
		}
		candidate := request{r.module, v}
		entry := a.cache.supports(a.lookupDirs[0], r.module, v)
		a.reportMetadata(candidate, entry)
		if entry.status != 0 {
			continue
		}
		a.ui.endProgress()
		a.ui.text("  Trying earlier version %s...\n", candidate)
		a.ui.beginTask("Validating earlier candidate")
		ok, err := a.tryUpgrade([]request{candidate})
		a.ui.endProgress()
		if err != nil {
			return err
		}
		if ok {
			if err := a.acceptGraph(); err != nil {
				return err
			}
			a.ui.status("OK", "Accepted %s", candidate)
			return nil
		}
		a.ui.status("WARN", "Rejected %s; restored the last validated graph", candidate)
		a.ui.beginMetadata(1, 1)
	}
	a.ui.status("SKIP", "Retained %s@%s: no newer candidate passed validation", r.module, previous)
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
	a.ui.text("  Validating dependency batch (%d modules)...\n", len(batch))
	ok, err := a.tryUpgrade(batch)
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
		a.ui.status("OK", "Accepted batch (%d modules)", len(batch))
		a.ui.detail("  Candidates: %s\n", strings.Join(requests, " "))
		return nil
	}
	a.ui.status("WARN", "Batch rejected; restored the last validated graph")
	if len(batch) == 1 {
		return a.fallback(batch[0])
	}
	split := batchSplit(batch)
	a.ui.text("  Splitting into %d and %d modules\n", split, len(batch)-split)
	if err := a.upgradeBatch(batch[:split]); err != nil {
		return err
	}
	return a.upgradeBatch(batch[split:])
}
