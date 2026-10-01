package main

import (
	"testing"
	"time"
)

func TestOptionsCompatibility(t *testing.T) {
	getenv := func(key string) string {
		return map[string]string{"HOME": "/home/test", "UPGRADE_GO_CACHE_TTL": "00060", "UPGRADE_GO_WORKERS": "8"}[key]
	}
	o, help, err := parseOptions([]string{"--skip-tests", "go1.25", "--upgrade-deps", "--refresh-cache", "--skip-vet", "--verbose"}, getenv)
	if err != nil || help {
		t.Fatalf("parse: %v, help=%t", err, help)
	}
	if o.target != "1.25.0" || !o.skipTests || !o.skipVet || !o.upgradeDeps || !o.refreshCache || !o.verbose {
		t.Fatalf("legacy flags lost: %+v", o)
	}
	if o.cacheRoot != "/home/test/.cache/upgrade-go" || o.cacheTTL != time.Minute || o.workers != 8 {
		t.Fatalf("environment settings lost: %+v", o)
	}
}

func TestOptionsRejectInvalidInput(t *testing.T) {
	for _, args := range [][]string{nil, {"1.25", "1.26"}, {"1.25rc1"}, {"1.25", "--keep-dep=example.com/lib"}, {"--unknown"}} {
		if _, _, err := parseOptions(args, func(string) string { return "" }); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	for _, env := range []map[string]string{
		{"UPGRADE_GO_CACHE_TTL": "-1"}, {"UPGRADE_GO_CACHE_TTL": "1000000000"},
		{"UPGRADE_GO_WORKERS": "0"}, {"UPGRADE_GO_WORKERS": "65"}, {"UPGRADE_GO_WORKERS": "text"},
	} {
		if _, _, err := parseOptions([]string{"1.25", "--upgrade-deps"}, func(key string) string { return env[key] }); err == nil {
			t.Errorf("accepted invalid configuration: %v", env)
		}
	}
	if _, help, err := parseOptions([]string{"--help"}, func(string) string { return "" }); err != nil || !help {
		t.Fatalf("help requires target: %v", err)
	}
}

func TestBuildCommandOptions(t *testing.T) {
	for _, args := range [][]string{
		{"1.25.3", "--build-command=make clean build", "--skip-tests"},
		{"--build-command", "make clean build", "1.25.3", "--skip-tests"},
	} {
		o, _, err := parseOptions(args, func(string) string { return "" })
		if err != nil || o.buildCommand != "make clean build" || o.target != "1.25.3" || !o.skipTests {
			t.Fatalf("custom build command lost: %+v, %v", o, err)
		}
	}
	for _, args := range [][]string{
		{"1.25.3", "--build-command"},
		{"1.25.3", "--build-command="},
		{"1.25.3", "--build-command", " \t "},
		{"1.25.3", "--build-command", "--skip-tests"},
	} {
		if _, _, err := parseOptions(args, func(string) string { return "" }); err == nil {
			t.Errorf("accepted missing or empty build command: %v", args)
		}
	}
}
