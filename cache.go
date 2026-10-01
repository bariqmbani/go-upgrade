package main

import (
	"crypto/sha256"
	"fmt"
	goversion "go/version"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

type cacheSource string

const (
	cacheMiss    cacheSource = "miss (queried Go)"
	cacheShared  cacheSource = "hit (shared)"
	cacheMemory  cacheSource = "hit (this run)"
	cacheRefresh cacheSource = "bypassed (--refresh-cache; queried Go)"
)

type catalog struct {
	latest     string
	candidates []string
	log        string
	source     cacheSource
}

type metadata struct {
	// 0: compatible metadata, 1: intrinsic incompatibility, 2: transient lookup failure.
	status int
	log    string
	source cacheSource
}

type dependencyCache struct {
	runner                    *runner
	target, shared            string
	refresh                   bool
	ttl                       time.Duration
	mu                        sync.Mutex
	versions                  map[string]catalog
	metadata                  map[string]metadata
	versionHits, metadataHits atomic.Int64
}

func hash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func (a *app) initializeCache() error {
	config, log, err := a.runner.query(a.dir, "env", "-json", "GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE", "GOAUTH", "GOFLAGS")
	if err != nil {
		fmt.Fprint(a.errOut, log)
		return err
	}
	root, err := filepath.Abs(a.opts.cacheRoot)
	if err != nil {
		return err
	}
	// Match the shell implementation byte-for-byte, including the final newline.
	scope := hash(a.localGo + "\n" + config + "\n")
	shared := filepath.Join(root, "v1", a.opts.target, scope)
	if err = os.MkdirAll(shared, 0o755); err == nil {
		var probe *os.File
		probe, err = os.CreateTemp(shared, ".write-check-*")
		if err == nil {
			probe.Close()
			os.Remove(probe.Name())
		}
	}
	if err != nil {
		fmt.Fprintln(a.errOut, "Warning: shared cache unavailable; using the per-run cache.")
		shared = ""
	} else {
		fmt.Fprintf(a.out, "    Shared cache: %s (version lists expire after %.0fs)\n", root, a.opts.cacheTTL.Seconds())
	}
	a.cache = &dependencyCache{
		runner: a.runner, target: a.opts.target, shared: shared,
		refresh: a.opts.refreshCache, ttl: a.opts.cacheTTL,
		versions: make(map[string]catalog), metadata: make(map[string]metadata),
	}
	return nil
}

func decodeCatalog(data []byte, ttl time.Duration, now time.Time) (catalog, bool) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 2 || len(lines[0]) > 12 || lines[0] == "" {
		return catalog{}, false
	}
	for _, ch := range lines[0] {
		if ch < '0' || ch > '9' {
			return catalog{}, false
		}
	}
	timestamp, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil || now.Unix() < timestamp || now.Unix()-timestamp >= int64(ttl/time.Second) || !semver.IsValid(lines[1]) {
		return catalog{}, false
	}
	c := catalog{latest: lines[1]}
	for _, v := range lines[2:] {
		if !semver.IsValid(v) {
			return catalog{}, false
		}
		c.candidates = append(c.candidates, v)
	}
	return c, true
}

func decodeMetadata(data []byte) (metadata, bool) {
	parts := strings.SplitN(string(data), "\n", 2)
	if len(parts) != 2 || (parts[0] != "0" && parts[0] != "1") {
		return metadata{}, false
	}
	status, _ := strconv.Atoi(parts[0])
	return metadata{status: status, log: parts[1]}, true
}

func (c *dependencyCache) catalog(dir, module string) catalog {
	c.mu.Lock()
	entry, exists := c.versions[module]
	c.mu.Unlock()
	if exists {
		entry.source = cacheMemory
		return entry
	}
	path := filepath.Join(c.shared, "versions", hash(module))
	if !c.refresh && c.shared != "" {
		if data, err := os.ReadFile(path); err == nil {
			entry, exists = decodeCatalog(data, c.ttl, time.Now())
			if exists {
				entry.source = cacheShared
				c.versionHits.Add(1)
			}
		}
	}
	if !exists {
		entry = c.fetchCatalog(dir, module, path)
		entry.source = cacheMiss
		if c.refresh {
			entry.source = cacheRefresh
		}
	}
	c.mu.Lock()
	c.versions[module] = entry
	c.mu.Unlock()
	return entry
}

func (c *dependencyCache) fetchCatalog(dir, module, path string) catalog {
	out, log, err := c.runner.query(dir, "list", "-m", "-versions", "-f", "{{range .Versions}}{{println .}}{{end}}", module)
	if err != nil {
		return catalog{log: fmt.Sprintf("    Version lookup failed for %s; retaining its current version.\n%s", module, log)}
	}
	entry := catalog{}
	versions := strings.Fields(out)
	latest, log, latestErr := c.runner.query(dir, "list", "-m", "-f", "{{.Version}}", module+"@latest")
	if latestErr != nil {
		entry.log = fmt.Sprintf("    Latest-version lookup failed for %s; checking listed releases.\n%s", module, log)
	} else if semver.IsValid(latest) {
		entry.latest = latest
		versions = append(versions, latest)
	}
	seen := make(map[string]bool)
	for _, v := range versions {
		if semver.IsValid(v) && !seen[v] {
			seen[v] = true
			entry.candidates = append(entry.candidates, v)
		}
	}
	sort.SliceStable(entry.candidates, func(i, j int) bool {
		return semver.Compare(entry.candidates[i], entry.candidates[j]) > 0
	})
	// Failed queries are retried by the next project; don't poison the shared cache.
	if c.shared != "" && latestErr == nil && entry.latest != "" {
		data := fmt.Sprintf("%d\n%s\n", time.Now().Unix(), entry.latest)
		for _, v := range entry.candidates {
			data += v + "\n"
		}
		if err := atomicWrite(path, []byte(data), 0o644); err != nil {
			entry.log += fmt.Sprintf("Warning: could not save shared versions for %s.\n", module)
		}
	}
	return entry
}

func (c *dependencyCache) supports(dir, module, version string) metadata {
	key := module + "@" + version
	c.mu.Lock()
	entry, exists := c.metadata[key]
	c.mu.Unlock()
	if exists {
		entry.source = cacheMemory
		return entry
	}
	path := filepath.Join(c.shared, "metadata", hash(key))
	if !c.refresh && c.shared != "" {
		if data, err := os.ReadFile(path); err == nil {
			entry, exists = decodeMetadata(data)
			if exists {
				entry.source = cacheShared
				c.metadataHits.Add(1)
			}
		}
	}
	if !exists {
		entry = c.fetchMetadata(dir, module, version)
		entry.source = cacheMiss
		if c.refresh {
			entry.source = cacheRefresh
		}
		if c.shared != "" && entry.status != 2 {
			data := fmt.Sprintf("%d\n%s", entry.status, entry.log)
			if err := atomicWrite(path, []byte(data), 0o644); err != nil {
				entry.log += fmt.Sprintf("Warning: could not save shared metadata for %s.\n", key)
			}
		}
	}
	c.mu.Lock()
	c.metadata[key] = entry
	c.mu.Unlock()
	return entry
}

func (c *dependencyCache) fetchMetadata(dir, module, version string) metadata {
	path, log, err := c.runner.query(dir, "list", "-m", "-f", "{{.GoMod}}", module+"@"+version)
	if err != nil {
		return metadata{status: 2, log: log + err.Error() + "\n"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return metadata{status: 2, log: fmt.Sprintf("Cannot read module metadata: %v\n", err)}
	}
	file, err := modfile.ParseLax(path, data, nil)
	if err != nil {
		return metadata{status: 2, log: fmt.Sprintf("Cannot parse module metadata: %v\n", err)}
	}
	if file.Module != nil && file.Module.Mod.Path != module {
		return metadata{status: 1, log: fmt.Sprintf("Module declares a different path: %s\n", file.Module.Mod.Path)}
	}
	if file.Go != nil && goversion.Compare("go"+normalizeGo(c.target), "go"+normalizeGo(file.Go.Version)) < 0 {
		return metadata{status: 1, log: fmt.Sprintf("Requires Go %s; target is %s.\n", file.Go.Version, c.target)}
	}
	return metadata{}
}
