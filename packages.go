package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type packageModule struct {
	Path, Version string
	Main          bool
}

type packageInfo struct {
	ImportPath, Dir, Name string
	Imports               []string
	Module                *packageModule
	Incomplete            bool
	Error                 *struct{ Err string }
	DepsErrors            []struct{ Err string }
}

type packageGraph struct {
	packages   map[string]*packageInfo
	incomplete bool
}

func packageID(path string) string {
	path, _, _ = strings.Cut(path, " [")
	return path
}

func decodePackages(out string) (*packageGraph, error) {
	g := &packageGraph{packages: make(map[string]*packageInfo)}
	decoder := json.NewDecoder(strings.NewReader(out))
	for {
		var p packageInfo
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading package relationships: %w", err)
		}
		g.incomplete = g.incomplete || p.Incomplete || p.Error != nil || len(p.DepsErrors) != 0
		p.ImportPath = packageID(p.ImportPath)
		for i := range p.Imports {
			p.Imports[i] = packageID(p.Imports[i])
		}
		if p.ImportPath == "" {
			g.incomplete = true
			continue
		}
		// Test variants share a package identity. Retain all their imports for
		// retry hints.
		if previous := g.packages[p.ImportPath]; previous != nil {
			previous.Imports = append(previous.Imports, p.Imports...)
		} else {
			g.packages[p.ImportPath] = &p
		}
	}
	if len(g.packages) == 0 {
		return nil, fmt.Errorf("package inspection returned no packages")
	}
	for _, p := range g.packages {
		for _, dependency := range p.Imports {
			if dependency != "C" && g.packages[dependency] == nil {
				g.incomplete = true
			}
		}
	}
	return g, nil
}

func (a *app) inspectPackages(tests bool) (*packageGraph, error) {
	args := []string{"list", "-deps", "-e", "-json"}
	if tests {
		args = append(args, "-test")
	}
	args = append(args, "./...")
	out, err := a.queryGo(args...)
	if err != nil {
		return nil, err
	}
	return decodePackages(out)
}

func (g *packageGraph) moduleEdges() map[string]map[string]bool {
	edges := make(map[string]map[string]bool)
	for _, p := range g.packages {
		if p.Module == nil || p.Module.Main {
			continue
		}
		from := p.Module.Path
		for _, path := range p.Imports {
			q := g.packages[path]
			if q == nil || q.Module == nil || q.Module.Main || q.Module.Path == from {
				continue
			}
			if edges[from] == nil {
				edges[from] = make(map[string]bool)
			}
			edges[from][q.Module.Path] = true
		}
	}
	return edges
}

func (g *packageGraph) changedModules(before map[string]string) map[string]bool {
	changed := make(map[string]bool)
	for _, p := range g.packages {
		if p.Module != nil && !p.Module.Main && p.Module.Version != before[p.Module.Path] {
			changed[p.Module.Path] = true
		}
	}
	return changed
}
