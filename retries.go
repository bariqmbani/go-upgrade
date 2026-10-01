package main

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type failurePoint struct {
	file, pkg, message string
	line, column       int
}

var sourceDiagnostic = regexp.MustCompile(`^(.+\.go):([0-9]+)(?::([0-9]+))?:\s*(.*)$`)
var qualifiedName = regexp.MustCompile(`\b([A-Za-z_][A-Za-z_0-9]*)\.[A-Za-z_][A-Za-z_0-9]*`)
var quotedImport = regexp.MustCompile(`"([^"\s]+)"`)

// Keep bounded hints, while runCommand still prints the entire original log.
// An unusual/oversized line can disable attribution without affecting validation.
func readFailurePoints(r io.Reader) []failurePoint {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var points []failurePoint
	pkg := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# ") {
			pkg = packageID(strings.TrimSpace(strings.TrimPrefix(line, "# ")))
		}
		if match := sourceDiagnostic.FindStringSubmatch(line); match != nil {
			n, _ := strconv.Atoi(match[2])
			column, _ := strconv.Atoi(match[3])
			points = append(points, failurePoint{match[1], pkg, match[4], n, column})
			if len(points) == 256 {
				break
			}
		}
	}
	return points
}

type retryGuide struct {
	graph    *packageGraph
	problems map[string]bool
}

func (g *packageGraph) sourceOwner(dir string, point failurePoint) (*packageInfo, string) {
	file := point.file
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	if p := g.packages[point.pkg]; p != nil {
		// A compiler may print a filename relative to its package directory.
		if filepath.Dir(file) != p.Dir && !filepath.IsAbs(point.file) {
			file = filepath.Join(p.Dir, point.file)
		}
		return p, file
	}
	for _, p := range g.packages {
		if p.Dir != "" && filepath.Dir(file) == p.Dir {
			return p, file
		}
	}
	return nil, file
}

func (g *packageGraph) referencedImports(file string, point failurePoint) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return nil
	}
	aliases := make(map[string]string)
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if p := g.packages[path]; p != nil && p.Name != "" {
			name = p.Name
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		aliases[name] = path
	}
	selected := make(map[string]bool)
	for _, match := range qualifiedName.FindAllStringSubmatch(point.message, -1) {
		if path := aliases[match[1]]; path != "" {
			selected[path] = true
		}
	}
	// Source positions disambiguate multiple calls on a single line.
	ast.Inspect(f, func(node ast.Node) bool {
		s, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		start, end := fset.Position(s.Pos()), fset.Position(s.End())
		if start.Line == point.line && point.column >= start.Column && point.column < end.Column {
			if name, ok := s.X.(*ast.Ident); ok && aliases[name.Name] != "" {
				selected[aliases[name.Name]] = true
			}
		}
		return true
	})
	for _, match := range quotedImport.FindAllStringSubmatch(point.message, -1) {
		if g.packages[match[1]] != nil {
			selected[match[1]] = true
		}
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func newRetryGuide(g *packageGraph, before map[string]string, points []failurePoint, dir string) *retryGuide {
	guide := &retryGuide{graph: g, problems: make(map[string]bool)}
	changed := g.changedModules(before)
	for _, point := range points {
		owner, file := g.sourceOwner(dir, point)
		if owner != nil && owner.Module != nil && changed[owner.Module.Path] {
			guide.problems[owner.Module.Path] = true
		}
		roots := g.referencedImports(file, point)
		if len(roots) == 0 && owner != nil && owner.Module != nil && !owner.Module.Main {
			roots = owner.Imports
		}
		seen := make(map[string]bool)
		for i := 0; i < len(roots); i++ {
			path := roots[i]
			if seen[path] {
				continue
			}
			seen[path] = true
			p := g.packages[path]
			if p == nil {
				continue
			}
			if p.Module != nil && changed[p.Module.Path] {
				guide.problems[p.Module.Path] = true
				// Stop at the nearest changed dependency. Walking farther would
				// blame shared utility modules for an API error at this boundary.
				continue
			}
			roots = append(roots, p.Imports...)
		}
	}
	return guide
}

// Candidate relationships are undirected for grouping. The main module is
// excluded: otherwise unrelated dependencies would all be joined through it.
func (g *retryGuide) groups(batch []request) [][]request {
	parent := make(map[string]string)
	for _, r := range batch {
		parent[r.module] = r.module
	}
	var find func(string) string
	find = func(path string) string {
		if parent[path] != path {
			parent[path] = find(parent[path])
		}
		return parent[path]
	}
	if g != nil {
		for from, dependencies := range g.graph.moduleEdges() {
			for to := range dependencies {
				if parent[from] == "" || parent[to] == "" {
					continue
				}
				a, b := find(from), find(to)
				if a > b {
					a, b = b, a
				}
				parent[b] = a
			}
		}
	}
	byRoot := make(map[string][]request)
	for _, r := range batch {
		root := find(r.module)
		byRoot[root] = append(byRoot[root], r)
	}
	var keys []string
	for root := range byRoot {
		keys = append(keys, root)
	}
	sort.Strings(keys)
	groups := make([][]request, 0, len(keys))
	for _, root := range keys {
		group := byRoot[root]
		sort.Slice(group, func(i, j int) bool { return group[i].module < group[j].module })
		groups = append(groups, group)
	}
	return groups
}

func reachesProblem(path string, edges map[string]map[string]bool, problems, seen map[string]bool) bool {
	if seen[path] {
		return false
	}
	seen[path] = true
	if problems[path] {
		return true
	}
	for dependency := range edges[path] {
		if reachesProblem(dependency, edges, problems, seen) {
			return true
		}
	}
	return false
}

func (g *retryGuide) split(batch []request) (left, right []request, reason string) {
	groups := g.groups(batch)
	if g != nil && len(g.problems) != 0 {
		edges := g.graph.moduleEdges()
		for _, group := range groups {
			for _, r := range group {
				// Error attribution takes precedence over broad components:
				// sharing a utility dependency does not implicate a candidate.
				if reachesProblem(r.module, edges, g.problems, make(map[string]bool)) {
					right = append(right, r)
				} else {
					left = append(left, r)
				}
			}
		}
		if len(left) != 0 && len(right) != 0 {
			return left, right, "compiler-guided; retrying implicated dependencies last"
		}
	}
	left, right = nil, nil
	var ordered []request
	boundary, distance := 0, len(batch)
	for i, group := range groups {
		ordered = append(ordered, group...)
		if i+1 < len(groups) {
			d := abs(len(batch) - 2*len(ordered))
			if d < distance {
				boundary, distance = len(ordered), d
			}
		}
	}
	if boundary == 0 {
		boundary = len(ordered) / 2
		return ordered[:boundary], ordered[boundary:], "balanced; no smaller dependency group"
	}
	return ordered[:boundary], ordered[boundary:], "dependency groups"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
