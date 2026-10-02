package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestInspectionCompletenessAndTestVariants(t *testing.T) {
	for _, input := range []string{
		`{"ImportPath":"root","Imports":["missing"]}`,
		`{"ImportPath":"root","Incomplete":true}`,
		`{"ImportPath":"root","Error":{"Err":"lookup failed"}}`,
		`{"ImportPath":"root","DepsErrors":[{"Err":"lookup failed"}]}`,
	} {
		g, err := decodePackages(input)
		if err != nil || !g.incomplete {
			t.Fatalf("incomplete inspection accepted: %s (%v)", input, err)
		}
	}
	g, err := decodePackages(`
{"ImportPath":"root","Imports":["C"]}
{"ImportPath":"root [root.test]","Imports":["testdep"]}
{"ImportPath":"testdep"}
`)
	if err != nil || g.incomplete || !reflect.DeepEqual(g.packages["root"].Imports, []string{"C", "testdep"}) {
		t.Fatalf("test variant imports lost: %+v, %v", g, err)
	}
	for _, input := range []string{"", `{"ImportPath":`} {
		if _, err := decodePackages(input); err == nil {
			t.Fatalf("invalid inspection accepted: %q", input)
		}
	}
}

func TestFailureHintsKeepCompilerSourceLocations(t *testing.T) {
	points := readFailurePoints(strings.NewReader("make: running build\n# example.com/app [example.com/app.test]\n./main.go:7:21: undefined: api.Value\nmake: *** failed\n"))
	want := []failurePoint{{file: "./main.go", pkg: "example.com/app", message: "undefined: api.Value", line: 7, column: 21}}
	if !reflect.DeepEqual(points, want) {
		t.Fatalf("failure attribution: %+v", points)
	}
}
