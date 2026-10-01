package main

import (
	"context"
	"fmt"
	goversion "go/version"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

type timing struct {
	name string
	time time.Duration
}

type app struct {
	opts               options
	ctx                context.Context
	out, errOut        io.Writer
	runner             *runner
	dir, work, localGo string
	step               string
	cache              *dependencyCache
	original, accepted graph
	baseline           map[string]string
	examined           map[string]bool
	timings            []timing
	lookupDirs         []string
}

func (a *app) phase(name, step string, fn func() error) error {
	a.step = step
	start := time.Now()
	err := fn()
	for i := range a.timings {
		if a.timings[i].name == name {
			a.timings[i].time += time.Since(start)
			return err
		}
	}
	a.timings = append(a.timings, timing{name, time.Since(start)})
	return err
}

func (a *app) run() (result error) {
	start := time.Now()
	a.step = "checking Go installation"
	var err error
	a.runner, err = newRunner(a.ctx)
	if err != nil {
		return err
	}
	a.dir, err = os.Getwd()
	if err != nil {
		return err
	}
	a.step = "reading local Go version"
	local, log, err := a.runner.query(a.dir, "env", "GOVERSION")
	if err != nil {
		fmt.Fprint(a.errOut, log)
		return err
	}
	a.localGo = normalizeGo(local)
	fmt.Fprintln(a.out, "========================================\nGo project upgrade\n========================================")
	fmt.Fprintf(a.out, "Local Go     : %s\nTarget Go    : %s\nToolchain    : local\nSkip tests   : %t\nSkip vet     : %t\nUpgrade deps : %t\n\n", a.localGo, a.opts.target, a.opts.skipTests, a.opts.skipVet, a.opts.upgradeDeps)
	a.step = "validating local Go version"
	if !goversion.IsValid("go" + a.localGo) {
		return fmt.Errorf("cannot determine a release version from local Go %q", local)
	}
	if goversion.Compare("go"+a.localGo, "go"+a.opts.target) < 0 {
		return fmt.Errorf("local Go (%s) is older than target (%s); install Go %s or newer first", a.localGo, a.opts.target, a.opts.target)
	}
	a.step = "locking project"
	lock, err := os.Open(a.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another upgrade is already running in this project: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	a.step = "checking go.mod"
	a.original, err = snapshot(a.dir)
	if err != nil {
		return fmt.Errorf("read module files (run from the module root): %w", err)
	}
	defer func() {
		if result != nil {
			if restoreErr := a.original.restore(a.dir); restoreErr != nil {
				result = fmt.Errorf("%w; restoring original module files failed: %v", result, restoreErr)
			} else {
				fmt.Fprintln(a.errOut, "Restored original go.mod and go.sum.")
			}
		}
	}()
	a.work, err = os.MkdirTemp("", "go-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(a.work)
	if err = a.phase("Baseline", "baseline build", func() error {
		fmt.Fprintf(a.out, "==> Running baseline build with local Go %s...\n", a.localGo)
		if err := a.build(a.out, false); err != nil {
			return err
		}
		if !a.opts.skipTests {
			a.step = "baseline tests"
			fmt.Fprintln(a.out, "==> Running baseline tests...")
			return a.runner.goRun(a.dir, a.out, "test", "-count=1", "./...")
		}
		return nil
	}); err != nil {
		return err
	}
	if err = a.phase("Target setup", "setting target Go version", func() error {
		fmt.Fprintf(a.out, "\n==> Setting go.mod target to Go %s...\n", a.opts.target)
		if err := a.runner.goRun(a.dir, a.out, "mod", "edit", "-go="+a.opts.target); err != nil {
			return err
		}
		a.step = "initial module tidy"
		fmt.Fprintln(a.out, "==> Tidying module...")
		return a.tidy(a.out)
	}); err != nil {
		return err
	}
	if a.opts.upgradeDeps {
		if err = a.phase("Target setup", "validating target baseline", func() error {
			if err := a.validate(a.out); err != nil {
				return err
			}
			return a.acceptGraph()
		}); err != nil {
			return err
		}
		a.baseline = a.accepted.modules
		a.examined = make(map[string]bool)
		if err = a.phase("Discovery", "initializing shared dependency cache", a.initializeCache); err != nil {
			return err
		}
		if err = a.makeLookups(); err != nil {
			return err
		}
		for {
			var batch []request
			if err = a.phase("Discovery", "discovering dependency upgrades", func() error {
				var err error
				batch, err = a.collectBatch()
				return err
			}); err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			if err = a.phase("Upgrade validation", "upgrading dependency batches", func() error { return a.upgradeBatch(batch) }); err != nil {
				return err
			}
		}
	}
	if err = a.phase("Final validation", "verifying go.mod target", a.finalValidation); err != nil {
		return err
	}
	a.summary(time.Since(start))
	return nil
}

func (a *app) build(output io.Writer, intermediate bool) error {
	if intermediate && a.opts.checkCommand != "" {
		return a.runner.run(a.dir, output, output, "/bin/sh", "-c", a.opts.checkCommand)
	}
	if a.opts.nds {
		return a.runner.run(a.dir, output, output, "make", "clean", "build")
	}
	return a.runner.goRun(a.dir, output, "build", "./...")
}

func (a *app) tidy(output io.Writer) error {
	if err := a.runner.goRun(a.dir, output, "mod", "tidy", "-go="+a.opts.target); err != nil {
		return err
	}
	return a.checkTarget()
}

func (a *app) checkTarget() error {
	file, err := readMod(a.dir)
	if err != nil {
		return err
	}
	actual := ""
	if file.Go != nil {
		actual = file.Go.Version
	}
	if actual != a.opts.target {
		return fmt.Errorf("go.mod Go version mismatch: expected %s, actual %s", a.opts.target, actual)
	}
	return nil
}

func (a *app) validate(output io.Writer) error {
	if err := a.build(output, true); err != nil {
		return err
	}
	if !a.opts.skipTests {
		if err := a.runner.goRun(a.dir, output, "test", "-count=1", "./..."); err != nil {
			return err
		}
		if err := a.runner.goRun(a.dir, output, "test", "-race", "-count=1", "./..."); err != nil {
			return err
		}
	}
	if !a.opts.skipVet {
		return a.runner.goRun(a.dir, output, "vet", "./...")
	}
	return nil
}

func (a *app) finalValidation() error {
	if err := a.checkTarget(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\n==> Verifying go.mod Go version...\n    go.mod: go %s\n", a.opts.target)
	a.step = "verifying modules"
	fmt.Fprintln(a.out, "==> Verifying modules...")
	if err := a.runner.goRun(a.dir, a.out, "mod", "verify"); err != nil {
		return err
	}
	if !a.opts.skipTests {
		a.step = "running tests with local Go"
		fmt.Fprintf(a.out, "==> Running tests with Go %s...\n", a.localGo)
		if err := a.runner.goRun(a.dir, a.out, "test", "-count=1", "./..."); err != nil {
			return err
		}
		a.step = "running race tests with local Go"
		fmt.Fprintf(a.out, "==> Running race tests with Go %s...\n", a.localGo)
		if err := a.runner.goRun(a.dir, a.out, "test", "-race", "-count=1", "./..."); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(a.out, "==> Tests skipped.")
	}
	if !a.opts.skipVet {
		a.step = "running go vet with local Go"
		fmt.Fprintf(a.out, "==> Running go vet with Go %s...\n", a.localGo)
		if err := a.runner.goRun(a.dir, a.out, "vet", "./..."); err != nil {
			fmt.Fprintln(a.errOut, "Vet checks source and test compilation even with --skip-tests.")
			return err
		}
	} else {
		fmt.Fprintln(a.out, "==> Vet skipped (--skip-vet).")
	}
	a.step = "final build validation with local Go"
	fmt.Fprintf(a.out, "==> Performing final build with Go %s...\n", a.localGo)
	if err := a.build(a.out, false); err != nil {
		return err
	}
	a.step = "final go.mod validation"
	return a.checkTarget()
}

func (a *app) summary(elapsed time.Duration) {
	tests, vet := "yes", "passed"
	if a.opts.skipTests {
		tests = "no"
	}
	if a.opts.skipVet {
		vet = "skipped"
	}
	fmt.Fprintln(a.out, "\n========================================\nSUCCESS\n========================================")
	fmt.Fprintf(a.out, "Local Go      : %s\nTarget Go     : %s\nFinal go.mod  : go %s\nToolchain     : local\nUpgrade deps  : %t\nTests run     : %s\nVet           : %s\nBuild         : passed\n", a.localGo, a.opts.target, a.opts.target, a.opts.upgradeDeps, tests, vet)
	if a.cache != nil {
		fmt.Fprintf(a.out, "Cache hits    : %d version lists, %d metadata checks\n", a.cache.versionHits.Load(), a.cache.metadataHits.Load())
	}
	fmt.Fprintf(a.out, "Elapsed       : %s\n========================================\n", elapsed.Round(time.Millisecond))
	fmt.Fprintln(a.out, "\nPhase timings:")
	for _, t := range a.timings {
		fmt.Fprintf(a.out, "  %-20s %s\n", t.name, t.time.Round(time.Millisecond))
	}
	if a.opts.upgradeDeps {
		fmt.Fprintln(a.out, "\nDependency version changes:")
		paths := make([]string, 0, len(a.accepted.modules))
		for path := range a.accepted.modules {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		changed := false
		for _, path := range paths {
			after, before := a.accepted.modules[path], a.baseline[path]
			if before == after {
				continue
			}
			if before == "" {
				before = "added"
			}
			fmt.Fprintf(a.out, "  %s: %s -> %s\n", path, before, after)
			changed = true
		}
		if !changed {
			fmt.Fprintln(a.out, "  No dependency versions changed.")
		}
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
