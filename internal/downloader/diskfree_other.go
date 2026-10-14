//go:build !(linux || darwin || freebsd)

package downloader

// statfsFree is unsupported on this platform: free space is reported unknown,
// which disables the free-space gate rather than blocking downloads.
func statfsFree(string) (uint64, bool) { return 0, false }
