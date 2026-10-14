package downloader

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFreeSpaceWalksUpToExistingAncestor(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" {
		t.Skip("free space is not measured on this platform")
	}
	dir := t.TempDir()
	free, ok := FreeSpace(filepath.Join(dir, "not", "created", "yet"))
	if !ok || free == 0 {
		t.Fatalf("FreeSpace(missing subdir) = (%d, %v), want the temp dir's free space", free, ok)
	}
	if _, ok := FreeSpace(""); ok {
		t.Fatal("FreeSpace(\"\") should be unknown")
	}
}

func TestRemainingBytes(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(partial, make([]byte, 30), 0o644); err != nil {
		t.Fatal(err)
	}
	got := RemainingBytes([]File{
		{Dest: partial, Size: 100},                    // 70 left
		{Dest: filepath.Join(dir, "b.mkv"), Size: 50}, // nothing on disk yet
		{Dest: filepath.Join(dir, "c.mkv"), Size: 0},  // unknown size: ignored
		{Dest: partial, Size: 10},                     // oversize partial: nothing left, never negative
	})
	if got != 120 {
		t.Fatalf("RemainingBytes = %d, want 120", got)
	}
}
