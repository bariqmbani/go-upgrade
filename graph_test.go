package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreModuleFiles(t *testing.T) {
	for _, hasSum := range []bool{false, true} {
		dir := t.TempDir()
		mod := filepath.Join(dir, "go.mod")
		sum := filepath.Join(dir, "go.sum")
		original := "module example.com/app\n\ngo 1.25.0\n"
		if err := os.WriteFile(mod, []byte(original), 0o640); err != nil {
			t.Fatal(err)
		}
		if hasSum {
			if err := os.WriteFile(sum, []byte("original sums\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		g, err := snapshot(dir)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(mod, []byte("changed"), 0o644)
		os.WriteFile(sum, []byte("new sums"), 0o644)
		if err := g.restore(dir); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(mod)
		info, _ := os.Stat(mod)
		if string(data) != original || info.Mode().Perm() != 0o640 {
			t.Fatal("original go.mod bytes or permissions not restored")
		}
		data, err = os.ReadFile(sum)
		if hasSum && (err != nil || string(data) != "original sums\n") {
			t.Fatal("original go.sum not restored")
		}
		if !hasSum && !os.IsNotExist(err) {
			t.Fatal("new go.sum left behind")
		}
	}
}

func TestSemanticVersionOrder(t *testing.T) {
	for _, pair := range [][2]string{
		{"v1.10.0", "v1.9.0"}, {"v1.1.0", "v1.1.0-rc.1"},
		{"v1.1.0-rc.10", "v1.1.0-rc.2"},
		{"v0.0.0-20260902000000-aaaaaaaaaaaa", "v0.0.0-20260901000000-bbbbbbbbbbbb"},
	} {
		if !newer(pair[0], pair[1]) || newer(pair[1], pair[0]) {
			t.Errorf("wrong version ordering: %v", pair)
		}
	}
	if newer("v1.0.0+incompatible", "v1.0.0") || newer("invalid", "v1.0.0") {
		t.Fatal("non-upgrade considered newer")
	}
}
