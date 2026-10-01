package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

type savedFile struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

type graph struct {
	mod, sum savedFile
	modules  map[string]string
	required map[string]bool
}

func readSaved(path string, optional bool) (savedFile, error) {
	info, err := os.Stat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return savedFile{}, nil
	}
	if err != nil {
		return savedFile{}, err
	}
	data, err := os.ReadFile(path)
	return savedFile{data: data, mode: info.Mode().Perm(), exists: true}, err
}

func snapshot(dir string) (graph, error) {
	g := graph{}
	var err error
	g.mod, err = readSaved(filepath.Join(dir, "go.mod"), false)
	if err != nil {
		return g, err
	}
	g.sum, err = readSaved(filepath.Join(dir, "go.sum"), true)
	return g, err
}

// Readers always see a complete entry, even across simultaneous projects.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".upgrade-go-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (g graph) restore(dir string) error {
	var errs []error
	for _, entry := range []struct {
		name string
		file savedFile
	}{{"go.mod", g.mod}, {"go.sum", g.sum}} {
		path := filepath.Join(dir, entry.name)
		if entry.file.exists {
			if err := atomicWrite(path, entry.file.data, entry.file.mode); err != nil {
				errs = append(errs, err)
			}
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func readMod(dir string) (*modfile.File, error) {
	path := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return modfile.Parse(path, data, nil)
}

type listedModule struct {
	Path, Version string
	Main          bool
	Replace       *listedModule
}

func (a *app) listModules() ([]listedModule, error) {
	out, err := a.queryGo("list", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	var modules []listedModule
	for {
		var m listedModule
		err := decoder.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading module graph: %w", err)
		}
		if !m.Main && m.Version != "" {
			modules = append(modules, m)
		}
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })
	return modules, nil
}

func (a *app) acceptGraph() error {
	g, err := snapshot(a.dir)
	if err != nil {
		return err
	}
	modules, err := a.listModules()
	if err != nil {
		return err
	}
	file, err := readMod(a.dir)
	if err != nil {
		return err
	}
	g.modules, g.required = make(map[string]string), make(map[string]bool)
	for _, m := range modules {
		g.modules[m.Path] = m.Version
	}
	for _, r := range file.Require {
		g.required[r.Mod.Path] = true
	}
	a.accepted = g
	return nil
}

func newer(a, b string) bool { return semver.IsValid(a) && semver.Compare(a, b) > 0 }
