# go-upgrade

Upgrade a Go module's declared Go version and optionally select newer dependencies
that pass validation in that project. Built with Go 1.27.1 for Linux/Unix.

## Installation

Requires Go 1.27.1 or newer.

```sh
go install github.com/bariqmbani/go-upgrade@latest
```

The binary is installed into `GOBIN` if set, otherwise the `bin` directory of
`GOPATH` (normally `$HOME/go/bin`). Add that directory to your `PATH`. For the
default location:

```sh
export PATH="$HOME/go/bin:$PATH"
go-upgrade --help
```

To install directly into `~/.local/bin`:

```sh
mkdir -p "$HOME/.local/bin"
GOBIN="$HOME/.local/bin" go install github.com/bariqmbani/go-upgrade@latest
```

### Install from source

```sh
git clone https://github.com/bariqmbani/go-upgrade.git
cd go-upgrade
go install .
```

Alternatively, `make install` backs up existing commands and installs
`go-upgrade` plus the `upgrade-go` alias into `~/.local/bin`. This method also
requires `make` and Python 3. `go install` installs only `go-upgrade`.

## Usage

```sh
go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps
go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps --build-command="make clean build"
```

The installed local Go must be at least the requested target. Every subprocess uses
`GOWORK=off GOTOOLCHAIN=local`; the command never downloads a target toolchain.

## Flags

| Flag | Behavior |
| --- | --- |
| `--skip-tests` | Skip regular and race tests; vet can still compile tests |
| `--skip-vet` | Skip vet, while keeping build validation |
| `--upgrade-deps` | Enable dependency upgrades; off by default |
| `--refresh-cache` | Refresh version catalogs and dependency metadata |
| `--verbose` | Show each candidate, cache source, and successful command output |
| `--build-command="command"` | Customize builds; default: `go build ./...` |
| `-h`, `--help` | Show usage |

Run from a module root. Targets accept `1.25`, `1.25.3`, or `go1.25.3`.
Tidy uses `go mod tidy -go=<target>`. Major module-path migrations and updates
to replaced modules are not automatic. Dependency versions can change during
tidy even without `--upgrade-deps`.

## Output

The default output shows configuration, phase results, dependency changes, cache
reuse, and timings. Successful build and test output is quiet. Supported terminals
show a two-line footer pinned to the bottom, with one blank row above it:

```text

Overall [####------] ~45% | Metadata 12/48 (25%)
example.com/module@v1.8.0 | Cached (shared)       | Elapsed       4s
```

The first line shows the overall progress bar and current phase's completed/total
count and percentage. The second shows an active dependency or check, its cache
source when relevant, and phase elapsed time in a fixed field on the right. The
timer updates once per second, independently of changing action labels. Log colors
are preserved unless `NO_COLOR` is set. Logs appear in a scrollable pane
above the footer. Mouse-wheel scrolling, Up/Down, Page Up/Page Down, and Home
browse earlier output without moving the footer. Scrolling upward pauses automatic
following; click **Go to bottom** or press **End** or **G** to return to the latest
output and resume following. Progress remains visible during fast
output and diagnostics, with only changed rows refreshed at most ten times per
second. Long module paths and the bar adapt to terminal size.

The interactive view uses a separate terminal screen and temporary disk-backed
logs. On completion, failure, or interruption, it restores the original screen
and prints the complete transcript once into normal terminal scrollback. During
the run, use the application's scroll controls; native terminal scrollback is
managed by your terminal emulator.

`~` marks the overall percentage as an estimate of completed work, not remaining
time. Enabled stages have equal weight: baseline, target setup, dependency
discovery, dependency validation, and final validation. Without `--upgrade-deps`,
only baseline, target setup, and final validation contribute. Overall progress
never moves backward when new dependencies or retries add work, stays below 100%
until final validation succeeds, and reports 100% in the success summary.

Metadata counts are exact for the current discovery pass; cache hits count too.
Additional discovery passes are numbered. Dependency validation counts each
dependency once when accepted, retained after fallback, or resolved by another
upgrade. Failed attempts do not advance that count. Other phases count completed
enabled checks. During a running build or test, its count stays steady while
elapsed time updates.

Use `--verbose` for individual candidate results and successful command transcripts:

```sh
go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps --verbose
```

Errors **always include complete diagnostics**, including failed candidates or
batches before fallback. Command failures include the command, exit code, and
full output; candidate failures also identify the dependencies being validated.
Diagnostics go to stderr. Successful phase results and summaries go to stdout.

Redirected output or stdin, CI, `TERM=dumb`, unknown dimensions, and terminals smaller than
60 columns or 8 rows use plain count/percentage summaries without animation. Long
steps report progress every ten seconds. `NO_COLOR` disables status colors while
retaining terminal progress. Interactive mode requires stdin and stdout on the
same terminal. If the terminal becomes too small, the transcript is printed and
the rest of the run uses plain output. Terminal input settings, cursor visibility,
and mouse reporting are restored on exit. For detailed plain logs, capture both streams:

```sh
go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps --verbose > upgrade.log 2>&1
```

## Performance and compatibility

Metadata discovery uses four concurrent workers by default, each in a separate
temporary module. Set `UPGRADE_GO_WORKERS=1` for sequential discovery or a value
up to 64 for greater concurrency. Dependency selection and upgrade order are
deterministic; live progress and verbose messages follow worker activity.

Module changes and builds stay sequential. Compatible candidates are attempted
in batches. A failing batch is split, and a failing individual dependency falls
back through older releases. Existing dependencies are not downgraded. Newly
introduced dependencies get another discovery pass.

The default build is `go build ./...`. Set `--build-command="make clean build"`
or `--build-command "make clean build"` to customize baseline, candidate, and final
build validation. The command runs via `/bin/sh -c` from the module root with
`GOWORK=off GOTOOLCHAIN=local`. Shell quoting, environment assignments, and command
chaining are supported.

For a Makefile whose preparation is done by `setup` and `protoc`, intermediate
validation can optionally reuse the files created by the baseline build:

```sh
UPGRADE_GO_CHECK_COMMAND='make -o setup -o protoc build' \
  go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps --build-command="make clean build"
```

Only set this command if it performs equivalent compilation for your project.
Baseline and final validation use `--build-command` or the default build. All enabled
tests and vet checks still run for intermediate candidates.

The success summary includes phase timings. Metadata checks establish module
path and declared minimum Go compatibility. Project builds and enabled checks
establish compatibility with the code exercised by those checks; they cannot
prove the absence of every behavior change. A newer local compiler does not
prove compilation with the exact target compiler.

## Shared cache

The shell scripts' existing **v1 format and keys are preserved**:

```
${UPGRADE_GO_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/upgrade-go}
  /v1/<target>/<resolver-and-local-Go-hash>/{versions,metadata}/<entry-hash>
```

`UPGRADE_GO_CACHE_TTL` controls version-list expiry in seconds (default 86400,
0 forces fresh discovery). Metadata for fixed versions is reused until
`--refresh-cache`. Resolver configuration is hashed, including the local Go
version, so upgrading the local toolchain starts a new cache scope.

Atomic cache publication supports concurrent independent projects. Lookup
failures are not persisted as incompatibility. An unwritable shared cache falls
back to in-memory storage. Cached metadata never skips a project's own builds.

The shared cache stores version catalogs and declared Go/module-path compatibility.
It does not store project-specific accepted versions or rejected build outcomes.
A second run in the same repository can reuse metadata and still retry the same
fallback builds. `--verbose` shows whether each metadata lookup was cached;
those cache labels do not imply reuse of project validation results.

Version-list lookups and candidate metadata checks report cache usage in the
live status line and in verbose logs:

```text
  Version list example.com/lib: Cached (shared)
  Candidate example.com/lib@v1.2.0: Cached (shared)
  Compatible: example.com/lib@v1.2.0 (Cached (shared))
```

`Cached (this run)` means an in-memory result was reused. `Checking` means the
command is querying Go, which may still use Go's own module cache. Version queries
appear as `Finding versions` in the live line. `Refreshing` means `--refresh-cache`
bypassed the shared cache. Candidate checks report the same information when
falling back to older versions. Cache decisions are reported before a query starts.

## Recovery and development

Failed or interrupted runs restore the original `go.mod` and `go.sum`, including
whether `go.sum` originally existed. Concurrent invocations in the same module
are rejected. Build commands may also change generated files or configuration;
those files are outside module-file rollback.

```sh
make check
make integration
make integration-race
make build
```

Integration tests use real modules from a temporary file proxy. They cover API
breaks and older-version fallback, test failures, new dependencies, shared-cache
reuse, cache expiry and corruption, transient lookup failures, custom build commands,
worker concurrency, replaced modules, interrupt rollback, and project locking.
Presentation tests use screen models, pipes, and real pseudo terminals to check
independent footer updates, scroll anchoring, the blank margin, percentages,
transcript replay, resizing, terminal fallbacks, full error output, keyboard input,
and interrupt cleanup.
The concurrency timing test injects metadata query latency to measure the worker
pool; it is not a benchmark of a production project. Optionally set
`UPGRADE_GO_LEGACY_SCRIPT` to a backed-up shell script to test cache sharing in
both directions against that implementation.

`make install` backs up existing commands under `backups/<UTC timestamp>/` before
installing. Set `UPGRADE_GO_INSTALL_DIR` to choose another executable directory.
Both backups and compiled binaries are ignored by Git.
