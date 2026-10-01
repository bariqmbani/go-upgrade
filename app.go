package main

import (
	"context"
	"errors"
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
	interrupt          func()
	out, errOut        io.Writer
	ui                 *presenter
	runner             *runner
	dir, work, localGo string
	step               string
	candidateContext   string
	cache              *dependencyCache
	original, accepted graph
	baseline           map[string]string
	examined           map[string]bool
	timings            []timing
	lookupDirs         []string
	failurePoints      []failurePoint
	buildAttempts      int
	precheckAttempts   int
	precheckRejected   int
}

func (a *app) phase(stage progressStage, name, step string, total int, fn func() error) error {
	a.step = step
	start := time.Now()
	// Discovery owns its progress so empty passes produce no extra headings.
	showPhase := name != "Dependency discovery"
	if showPhase {
		a.ui.beginPhase(stage, name, total)
	}
	err := fn()
	if err == nil && showPhase {
		a.ui.phaseDone()
	}
	elapsed := time.Since(start)
	a.ui.endProgress()
	status := "OK"
	if err != nil {
		status = "FAIL"
	}
	if showPhase {
		a.ui.status(status, "%s %s (%s)", name, a.ui.phaseCounts(), humanDuration(elapsed))
	}
	for i := range a.timings {
		if a.timings[i].name == name {
			a.timings[i].time += elapsed
			return err
		}
	}
	a.timings = append(a.timings, timing{name, elapsed})
	return err
}

func (a *app) run() (result error) {
	start := time.Now()
	a.ui = newPresenter(a.out, a.errOut, a.opts.verbose)
	a.ui.interrupt = a.interrupt
	a.ui.configure(a.opts.upgradeDeps)
	a.ui.start()
	defer a.ui.close()
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
	local, err := a.queryGo("env", "GOVERSION")
	if err != nil {
		return err
	}
	a.localGo = normalizeGo(local)
	tests, vet, deps := "Enabled", "Enabled", "Disabled"
	if a.opts.skipTests {
		tests = "Skipped"
	}
	if a.opts.skipVet {
		vet = "Skipped"
	}
	if a.opts.upgradeDeps {
		deps = "Enabled"
	}
	build := a.opts.buildCommand
	if build == "" {
		build = "go build ./..."
	}
	a.ui.text("Go project upgrade\n\n  Go:            %s installed -> %s target\n  Toolchain:     Local\n  Dependencies:  %s\n  Tests:         %s\n  Vet:           %s\n  Build command: %s\n", a.localGo, a.opts.target, deps, tests, vet, build)
	if a.opts.checkCommand != "" {
		a.ui.text("  Intermediate:  %s\n", a.opts.checkCommand)
	}
	if a.opts.compilePrecheck {
		a.ui.text("  Precheck:      Compile affected packages\n")
	}
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
			a.ui.endProgress()
			if restoreErr := a.original.restore(a.dir); restoreErr != nil {
				result = fmt.Errorf("%w; restoring original module files failed: %v", result, restoreErr)
			} else {
				a.ui.diagnostic("Restored original go.mod and go.sum.", "")
			}
		}
	}()
	a.work, err = os.MkdirTemp("", "go-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(a.work)
	if err = a.phase(stageBaseline, "Baseline checks", "baseline build", a.baselineUnits(), func() error {
		if err := a.build("baseline build", false); err != nil {
			return err
		}
		a.ui.checkDone()
		if !a.opts.skipTests {
			if err := a.runGo("baseline tests", "test", "-count=1", "./..."); err != nil {
				return err
			}
			a.ui.checkDone()
		}
		return nil
	}); err != nil {
		return err
	}
	if err = a.phase(stageTarget, "Target setup", "setting target Go version", a.targetUnits(), func() error {
		if err := a.runGo("setting target Go version", "mod", "edit", "-go="+a.opts.target); err != nil {
			return err
		}
		a.ui.checkDone()
		if err := a.tidy("initial module tidy"); err != nil {
			return err
		}
		a.ui.checkDone()
		if a.opts.upgradeDeps {
			if err := a.validate(); err != nil {
				return err
			}
			if err := a.acceptGraph(); err != nil {
				return err
			}
			a.ui.checkDone()
		}
		return nil
	}); err != nil {
		return err
	}
	if a.opts.upgradeDeps {
		a.baseline = a.accepted.modules
		a.examined = make(map[string]bool)
		if err = a.phase(stageDiscovery, "Dependency discovery", "initializing shared dependency cache", 0, a.initializeCache); err != nil {
			return err
		}
		if err = a.makeLookups(); err != nil {
			return err
		}
		for {
			var batch []request
			if err = a.phase(stageDiscovery, "Dependency discovery", "discovering dependency upgrades", 0, func() error {
				var err error
				batch, err = a.collectBatch()
				return err
			}); err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			if err = a.phase(stageDependencies, "Upgrade validation", "upgrading dependency batches", len(batch), func() error { return a.upgradeBatch(batch) }); err != nil {
				return err
			}
		}
	}
	if err = a.phase(stageFinal, "Final validation", "verifying go.mod target", a.finalUnits(), a.finalValidation); err != nil {
		return err
	}
	a.ui.success()
	a.summary(time.Since(start))
	return nil
}

// Spool command output to disk instead of keeping potentially large build logs
// in memory. Successful output is quiet by default; failures are always replayed
// in full before graph rollback or fallback.
func (a *app) runCommand(step, bin string, args ...string) error {
	a.step = step
	a.ui.task(step)
	log, err := os.CreateTemp(a.work, "command-*.log")
	if err != nil {
		return fmt.Errorf("capture output for %s: %w", step, err)
	}
	defer os.Remove(log.Name())
	defer log.Close()
	err = a.runner.run(a.dir, log, log, bin, args...)
	if _, seekErr := log.Seek(0, io.SeekStart); seekErr != nil {
		return errors.Join(err, fmt.Errorf("read command output: %w", seekErr))
	}
	if err != nil && a.candidateContext != "" {
		a.failurePoints = readFailurePoints(log)
		if _, seekErr := log.Seek(0, io.SeekStart); seekErr != nil {
			return errors.Join(err, fmt.Errorf("read command output: %w", seekErr))
		}
	}
	outputErr := a.ui.commandOutput(a.diagnosticLabel(step), commandName(bin, args...), log, err)
	return errors.Join(err, outputErr)
}

func (a *app) diagnosticLabel(step string) string {
	if a.candidateContext != "" {
		return step + "\n  Candidates: " + a.candidateContext
	}
	return step
}

func (a *app) runGo(step string, args ...string) error {
	return a.runCommand(step, a.runner.goBin, args...)
}

func (a *app) queryGo(args ...string) (string, error) {
	out, log, err := a.runner.query(a.dir, args...)
	if err != nil {
		outputErr := a.ui.commandOutput(a.diagnosticLabel(a.step), commandName(a.runner.goBin, args...), strings.NewReader(log), err)
		return out, errors.Join(err, outputErr)
	}
	a.ui.detail("%s", log)
	return out, nil
}

func (a *app) build(step string, intermediate bool) error {
	a.buildAttempts++
	if intermediate && a.opts.checkCommand != "" {
		return a.runCommand(step, "/bin/sh", "-c", a.opts.checkCommand)
	}
	if a.opts.buildCommand != "" {
		return a.runCommand(step, "/bin/sh", "-c", a.opts.buildCommand)
	}
	return a.runGo(step, "build", "./...")
}

func (a *app) tidy(step string) error {
	if err := a.runGo(step, "mod", "tidy", "-go="+a.opts.target); err != nil {
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

func (a *app) baselineUnits() int {
	if a.opts.skipTests {
		return 1
	}
	return 2
}

func (a *app) validationUnits() int {
	units := 1 // Build.
	if !a.opts.skipTests {
		units += 2 // Regular and race tests.
	}
	if !a.opts.skipVet {
		units++
	}
	return units
}

func (a *app) targetUnits() int {
	units := 2 // Edit and target-constrained tidy.
	if a.opts.upgradeDeps {
		units += a.validationUnits() + 1 // Validation and graph acceptance.
	}
	return units
}

func (a *app) finalUnits() int {
	return a.validationUnits() + 3 // Initial/final target checks and module verification.
}

func (a *app) validate() error {
	if err := a.build("intermediate build validation", true); err != nil {
		return err
	}
	a.ui.checkDone()
	if !a.opts.skipTests {
		if err := a.runGo("intermediate tests", "test", "-count=1", "./..."); err != nil {
			return err
		}
		a.ui.checkDone()
		if err := a.runGo("intermediate race tests", "test", "-race", "-count=1", "./..."); err != nil {
			return err
		}
		a.ui.checkDone()
	}
	if !a.opts.skipVet {
		if err := a.runGo("intermediate vet", "vet", "./..."); err != nil {
			return err
		}
		a.ui.checkDone()
	}
	return nil
}

func (a *app) finalValidation() error {
	if err := a.checkTarget(); err != nil {
		return err
	}
	a.ui.checkDone()
	if err := a.runGo("verifying modules", "mod", "verify"); err != nil {
		return err
	}
	a.ui.checkDone()
	if !a.opts.skipTests {
		if err := a.runGo("final tests", "test", "-count=1", "./..."); err != nil {
			return err
		}
		a.ui.checkDone()
		if err := a.runGo("final race tests", "test", "-race", "-count=1", "./..."); err != nil {
			return err
		}
		a.ui.checkDone()
	}
	if !a.opts.skipVet {
		if err := a.runGo("final vet", "vet", "./..."); err != nil {
			a.ui.diagnostic("Vet checks source and test compilation even with --skip-tests.", "")
			return err
		}
		a.ui.checkDone()
	}
	if err := a.build("final build validation", false); err != nil {
		return err
	}
	a.ui.checkDone()
	a.step = "final go.mod validation"
	if err := a.checkTarget(); err != nil {
		return err
	}
	a.ui.checkDone()
	return nil
}

func (a *app) summary(elapsed time.Duration) {
	tests, vet := "Passed", "Passed"
	if a.opts.skipTests {
		tests = "Skipped"
	}
	if a.opts.skipVet {
		vet = "Skipped"
	}
	a.ui.text("\n")
	a.ui.status("OK", "Upgrade complete: 100%% (%s)", humanDuration(elapsed))
	a.ui.text("  go.mod: %s | Build: Passed | Tests: %s | Vet: %s\n", a.opts.target, tests, vet)
	if a.cache != nil {
		a.ui.text("  Cache reuse: %d version lists, %d metadata checks\n", a.cache.versionHits.Load(), a.cache.metadataHits.Load())
	}
	a.ui.text("  Build validations: %d | Compile prechecks: %d (%d rejected)\n", a.buildAttempts, a.precheckAttempts, a.precheckRejected)
	a.ui.text("\nPhase timings\n")
	for _, t := range a.timings {
		a.ui.text("  %-22s %s\n", t.name, humanDuration(t.time))
	}
	if a.opts.upgradeDeps {
		a.ui.text("\nDependency changes\n")
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
			a.ui.text("  %s: %s -> %s\n", path, before, after)
			changed = true
		}
		if !changed {
			a.ui.text("  No dependency versions changed.\n")
		}
	}
}
