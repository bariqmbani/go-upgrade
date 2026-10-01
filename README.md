# go-upgrade

Upgrade a Go module's declared Go version and optionally select newer dependencies
that pass validation in that project. Built with Go 1.27.1 for Linux/Unix.

```sh
make install
go-upgrade 1.25.3 --skip-tests --skip-vet --upgrade-deps
go-upgrade-nds 1.25.3 --skip-tests --skip-vet --upgrade-deps
```

`upgrade-go` and `upgrade-go-nds` remain available as aliases. The installed local
Go must be at least the requested target. Every subprocess uses
`GOWORK=off GOTOOLCHAIN=local`; the command never downloads a target toolchain.

## Flags

| Flag | Behavior |
| --- | --- |
| `--skip-tests` | Skip regular and race tests; vet can still compile tests |
| `--skip-vet` | Skip vet, while keeping build validation |
| `--upgrade-deps` | Enable dependency upgrades; off by default |
| `--refresh-cache` | Refresh version catalogs and dependency metadata |
| `-h`, `--help` | Show usage |

Run from a module root. Targets accept `1.25`, `1.25.3`, or `go1.25.3`.
Tidy uses `go mod tidy -go=<target>`. Major module-path migrations and updates
to replaced modules are not automatic. Dependency versions can change during
tidy even without `--upgrade-deps`.

## Performance and compatibility

Metadata discovery uses four concurrent workers by default, each in a separate
temporary module. Set `UPGRADE_GO_WORKERS=1` for sequential discovery or a value
up to 64 for greater concurrency. Output and upgrade order are deterministic.

Module changes and builds stay sequential. Compatible candidates are attempted
in batches. A failing batch is split, and a failing individual dependency falls
back through older releases. Existing dependencies are not downgraded. Newly
introduced dependencies get another discovery pass.

The default build is `go build ./...`. The `-nds` command uses `make clean build`
for every build by default. The same binary handles both modes by command name.
For a Makefile whose preparation is done by `setup` and `protoc`, intermediate
validation can optionally reuse the files created by the baseline build:

```sh
UPGRADE_GO_CHECK_COMMAND='make -o setup -o protoc build' \
  go-upgrade-nds 1.25.3 --skip-tests --skip-vet --upgrade-deps
```

Only set this command if it performs equivalent compilation for your project.
Baseline and final validation still use the full default build. All enabled
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
reuse, cache expiry and corruption, transient lookup failures, NDS routing,
worker concurrency, replaced modules, interrupt rollback, and project locking.
The concurrency timing test injects metadata query latency to measure the worker
pool; it is not a benchmark of a production project. Optionally set
`UPGRADE_GO_LEGACY_SCRIPT` to a backed-up shell script to test cache sharing in
both directions against that implementation.

`make install` backs up existing commands under `backups/<UTC timestamp>/` before
installing. Set `UPGRADE_GO_INSTALL_DIR` to choose another executable directory.
Both backups and compiled binaries are ignored by Git.
