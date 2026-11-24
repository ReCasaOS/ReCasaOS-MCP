package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrepareStateDirCreatesAMissingOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "state")
	if err := PrepareStateDir(dir, func(error) { t.Error("unexpected warning") }); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("%v", err)
	}
}

func TestPrepareStateDirTightensAnExistingOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStateDir(dir, func(error) {}); err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix modes.
	if fi, _ := os.Stat(dir); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestPrepareStateDirFailsWhenItCannotBeMade(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStateDir(filepath.Join(f, "state"), func(error) {}); err == nil {
		t.Fatal("want an error")
	}
}
