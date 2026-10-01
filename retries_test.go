package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRetriesKeepCrossNamespaceDependenciesTogether(t *testing.T) {
	g, err := decodePackages(`
{"ImportPath":"a.example/client","Name":"client","Imports":["z.example/api"],"Module":{"Path":"a.example/client","Version":"v1.1.0"}}
{"ImportPath":"z.example/api","Name":"api","Module":{"Path":"z.example/api","Version":"v1.1.0"}}
{"ImportPath":"b.example/bad","Name":"bad","Module":{"Path":"b.example/bad","Version":"v1.1.0"}}
`)
	if err != nil {
		t.Fatal(err)
	}
	batch := []request{{"a.example/client", "v1.1.0"}, {"b.example/bad", "v1.1.0"}, {"z.example/api", "v1.1.0"}}
	guide := &retryGuide{graph: g}
	left, right, reason := guide.split(batch)
	if len(left) != 2 || left[0] != batch[0] || left[1] != batch[2] || len(right) != 1 || right[0] != batch[1] || reason != "dependency groups" {
		t.Fatalf("related candidates separated: %v / %v (%s)", left, right, reason)
	}
	guide.problems = map[string]bool{"z.example/api": true}
	left, right, _ = guide.split(batch)
	if !reflect.DeepEqual(left, batch[1:2]) || len(right) != 2 {
		t.Fatalf("API error should implicate its client too: %v / %v", left, right)
	}
}

func TestCompilerPositionIsolatesOneCallOnSharedLine(t *testing.T) {
	dir := t.TempDir()
	source := "package main\nimport (good \"remote.example/good\"; bad \"remote.example/bad\")\nfunc main() {good.Value(); bad.Value()}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := decodePackages(`
{"ImportPath":"example.com/app","Name":"main","Imports":["remote.example/good","remote.example/bad"],"Module":{"Path":"example.com/app","Main":true}}
{"ImportPath":"remote.example/good","Name":"lib","Imports":["shared.example/util"],"Module":{"Path":"remote.example/good","Version":"v1.1.0"}}
{"ImportPath":"remote.example/bad","Name":"lib","Imports":["shared.example/util"],"Module":{"Path":"remote.example/bad","Version":"v1.1.0"}}
{"ImportPath":"shared.example/util","Name":"util","Module":{"Path":"shared.example/util","Version":"v1.1.0"}}
`)
	if err != nil {
		t.Fatal(err)
	}
	g.packages["example.com/app"].Dir = dir
	before := map[string]string{"remote.example/good": "v1.0.0", "remote.example/bad": "v1.0.0", "shared.example/util": "v1.0.0"}
	points := []failurePoint{{file: "main.go", pkg: "example.com/app", line: 3, column: 30, message: "undefined: bad.Value"}}
	guide := newRetryGuide(g, before, points, dir)
	if !reflect.DeepEqual(guide.problems, map[string]bool{"remote.example/bad": true}) {
		t.Fatalf("unrelated call implicated: %v", guide.problems)
	}
	batch := []request{{"remote.example/good", "v1.1.0"}, {"remote.example/bad", "v1.1.0"}, {"shared.example/util", "v1.1.0"}}
	left, right, _ := guide.split(batch)
	if len(left) != 2 || !reflect.DeepEqual(right, batch[1:2]) {
		t.Fatalf("shared utilities implicated unrelated candidates: %v / %v", left, right)
	}
}

func TestUnknownFailuresAlwaysProduceSmallerBatches(t *testing.T) {
	var guide *retryGuide
	for n := 2; n <= 32; n++ {
		batch := make([]request, n)
		for i := range batch {
			batch[i] = request{module: string(rune('a' + i)), version: "v1.0.0"}
		}
		left, right, _ := guide.split(batch)
		if len(left) == 0 || len(right) == 0 || len(left)+len(right) != n {
			t.Fatalf("split makes no progress: %v / %v", left, right)
		}
	}
}
