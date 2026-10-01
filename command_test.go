package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCancelStopsBuildDescendants(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r, err := newRunner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = r.run(dir, io.Discard, io.Discard, "/bin/sh", "-c", "(sleep 0.5; echo late > child-write) & wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected cancellation error: %v", err)
	}
	time.Sleep(550 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "child-write")); !os.IsNotExist(err) {
		t.Fatal("descendant continued writing after canceled build")
	}
}
