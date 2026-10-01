package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReadShellCache(t *testing.T) {
	now := time.Unix(1800000000, 0)
	data := []byte("1799999990\nv1.2.0\nv1.2.0\nv1.1.0\n")
	c, ok := decodeCatalog(data, time.Minute, now)
	if !ok || c.latest != "v1.2.0" || len(c.candidates) != 2 || c.candidates[1] != "v1.1.0" {
		t.Fatalf("cannot read shell version catalog: %+v, %t", c, ok)
	}
	for _, data := range []string{"invalid\n", "1800000001\nv1.2.0\nv1.2.0\n", "1799999000\nv1.2.0\nv1.2.0\n", "1799999990\nv1.2.0\ngarbage\n"} {
		if _, ok := decodeCatalog([]byte(data), time.Minute, now); ok {
			t.Errorf("accepted expired or malformed cache: %q", data)
		}
	}
	if _, ok := decodeCatalog(data, 0, now); ok {
		t.Fatal("zero TTL reused version catalog")
	}
	for _, data := range []string{"0\n", "1\nRequires Go 1.27.0; target is 1.25.3.\n"} {
		if _, ok := decodeMetadata([]byte(data)); !ok {
			t.Fatalf("cannot read shell metadata entry: %q", data)
		}
	}
	if _, ok := decodeMetadata([]byte("2\nnetwork error\n")); ok {
		t.Fatal("accepted transient failure from shared metadata cache")
	}
}

func TestConcurrentCachePublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata", "entry")
	if err := atomicWrite(path, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				data := []byte(fmt.Sprintf("1\nworker %d entry %d\n", i, j))
				if err := atomicWrite(path, data, 0o644); err != nil {
					t.Error(err)
					return
				}
				read, err := os.ReadFile(path)
				if err != nil {
					t.Error(err)
					return
				}
				if _, ok := decodeMetadata(read); !ok {
					t.Errorf("read partial publication: %q", read)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v, %v", entries, err)
	}
}
