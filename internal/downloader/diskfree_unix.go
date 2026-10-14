//go:build linux || darwin || freebsd

package downloader

import "syscall"

// statfsFree returns the bytes available to an unprivileged user on the
// filesystem holding path.
func statfsFree(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
