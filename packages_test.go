package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestAffectedPackagesIncludesTransitiveConsumers(t *testing.T) {
	g, err := decodePackages(`
{"ImportPath":"remote.example/core","Name":"core","Module":{"Path":"remote.example/core","Version":"v1.1.0"}}
{"ImportPath":"remote.example/wrapper","Name":"wrapper","Imports":["remote.example/core"],"Module":{"Path":"remote.example/wrapper","Version":"v1.0.0"}}
{"ImportPath":"example.com/app/service","Name":"service","Imports":["remote.example/wrapper"],"Module":{"Path":"example.com/app","Main":true}}
{"ImportPath":"example.com/app/cmd","Name":"main","Imports":["example.com/app/service"],"Module":{"Path":"example.com/app","Main":true}}
{"ImportPath":"example.com/app/independent","Name":"independent","Module":{"Path":"example.com/app","Main":true}}
`)
	if err != nil || g.incomplete {
		t.Fatalf("package inspection: %v, %+v", err, g)
	}
	before := map[string]string{"remote.example/core": "v1.0.0", "remote.example/wrapper": "v1.0.0"}
	want := []string{"example.com/app/cmd", "example.com/app/service"}
	if got := g.affectedPackages(before); !reflect.DeepEqual(got, want) {
		t.Fatalf("affected consumers: %v, want %v", got, want)
	}
	before["remote.example/core"] = "v1.1.0"
	if got := g.affectedPackages(before); len(got) != 0 {
		t.Fatalf("unchanged graph needs no precheck: %v", got)
	}
	delete(before, "remote.example/core")
	if got := g.affectedPackages(before); !reflect.DeepEqual(got, want) {
		t.Fatalf("introduced dependency consumers: %v", got)
	}
}

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
