package main

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type options struct {
	target                    string
	skipTests, skipVet        bool
	upgradeDeps, refreshCache bool
	workers                   int
	cacheTTL                  time.Duration
	cacheRoot, checkCommand   string
	buildCommand              string
	verbose                   bool
}

var goNumber = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
var cacheSeconds = regexp.MustCompile(`^[0-9]{1,9}$`)

func normalizeGo(v string) string {
	v = strings.TrimPrefix(v, "go")
	if goNumber.MatchString(v) && strings.Count(v, ".") == 1 {
		return v + ".0"
	}
	return v
}

func parseOptions(args []string, getenv func(string) string) (options, bool, error) {
	o := options{workers: 4, cacheTTL: 24 * time.Hour}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if flag, value, equals := strings.Cut(arg, "="); flag == "--build-command" {
			if !equals {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					return o, false, fmt.Errorf("--build-command requires a command")
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return o, false, fmt.Errorf("--build-command requires a nonempty command")
			}
			o.buildCommand = value
			continue
		}
		switch arg {
		case "-h", "--help":
			return o, true, nil
		case "--skip-tests":
			o.skipTests = true
		case "--skip-vet":
			o.skipVet = true
		case "--upgrade-deps":
			o.upgradeDeps = true
		case "--refresh-cache":
			o.refreshCache = true
		case "--verbose":
			o.verbose = true
		default:
			if strings.HasPrefix(arg, "-") {
				return o, false, fmt.Errorf("unknown option: %s", arg)
			}
			if o.target != "" {
				return o, false, fmt.Errorf("multiple Go versions specified")
			}
			o.target = strings.TrimPrefix(arg, "go")
		}
	}
	if o.target == "" {
		return o, false, fmt.Errorf("Go version is required")
	}
	if !goNumber.MatchString(o.target) {
		return o, false, fmt.Errorf("invalid Go version: %s; expected 1.25 or 1.25.0", o.target)
	}
	o.target = normalizeGo(o.target)
	if o.upgradeDeps {
		if s := getenv("UPGRADE_GO_CACHE_TTL"); s != "" {
			if !cacheSeconds.MatchString(s) {
				return o, false, fmt.Errorf("UPGRADE_GO_CACHE_TTL must be a nonnegative integer (up to 9 digits)")
			}
			n, _ := strconv.ParseInt(s, 10, 64)
			o.cacheTTL = time.Duration(n) * time.Second
		}
		if s := getenv("UPGRADE_GO_WORKERS"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 64 {
				return o, false, fmt.Errorf("UPGRADE_GO_WORKERS must be an integer from 1 to 64")
			}
			o.workers = n
		}
	}
	o.cacheRoot = getenv("UPGRADE_GO_CACHE_DIR")
	if o.cacheRoot == "" {
		root := getenv("XDG_CACHE_HOME")
		if root == "" {
			root = filepath.Join(getenv("HOME"), ".cache")
		}
		o.cacheRoot = filepath.Join(root, "upgrade-go")
	}
	o.checkCommand = getenv("UPGRADE_GO_CHECK_COMMAND")
	return o, false, nil
}

func usage(w io.Writer, name string) {
	fmt.Fprintf(w, `Usage:
  %s <go-version> [--skip-tests] [--skip-vet] [--upgrade-deps] [--refresh-cache]
      [--build-command="command"] [--verbose]

Options:
  --skip-tests       Skip go test and race test (vet still checks tests)
  --skip-vet         Skip go vet; build validation still runs
  --upgrade-deps     Upgrade dependencies; disabled by default
  --refresh-cache    Refresh shared version lists and compatibility metadata
  --verbose          Show each candidate, cache source, and successful command output
  --build-command    Build command for baseline, candidates, and final validation
                     (default: go build ./...); runs via /bin/sh -c
  -h, --help         Show this help

Environment:
  UPGRADE_GO_CACHE_DIR      Shared cache directory (default: XDG cache/upgrade-go)
  UPGRADE_GO_CACHE_TTL      Version-list lifetime in seconds (default: 86400)
  UPGRADE_GO_WORKERS        Concurrent metadata workers (default: 4, maximum: 64)
  UPGRADE_GO_CHECK_COMMAND  Optional command for intermediate build validation

Notes:
  The installed Go must be at least the target; toolchain downloads are disabled.
  Tidy uses -go=<target>; it may adjust dependencies without --upgrade-deps.
  Upgrades validate batches, split failed batches, and try older releases.
  Cached metadata does not replace project build/test validation.
  Major module-path migrations are not automatic; replaced modules are skipped.
  Failed runs restore the original go.mod and go.sum.
  Build commands run from the module root with the local Go environment.
  Intermediate builds use --build-command unless UPGRADE_GO_CHECK_COMMAND is set.
`, filepath.Base(name))
}
