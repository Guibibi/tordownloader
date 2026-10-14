package downloader

import (
	"os"
	"path/filepath"
)

// FreeSpace reports the bytes available on the filesystem that holds (or will
// hold) path. path need not exist yet — a category folder is created on first
// download — so the nearest existing ancestor is measured instead. ok is false
// when free space can't be determined (unsupported platform, no existing
// ancestor, statfs error); callers should then not block on it.
func FreeSpace(path string) (free uint64, ok bool) {
	if path == "" {
		return 0, false
	}
	p := filepath.Clean(path)
	for {
		if _, err := os.Stat(p); err == nil {
			return statfsFree(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, false
		}
		p = parent
	}
}

// RemainingBytes returns how many more bytes downloading files would write:
// each file's expected size minus what is already on disk at its Dest (a
// resumed partial counts toward what's written). Files of unknown size (0)
// contribute nothing.
func RemainingBytes(files []File) int64 {
	var n int64
	for _, f := range files {
		if f.Size <= 0 {
			continue
		}
		left := f.Size
		if fi, err := os.Stat(f.Dest); err == nil {
			left -= fi.Size()
		}
		if left > 0 {
			n += left
		}
	}
	return n
}
