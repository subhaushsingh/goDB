//go:build windows

package minidb

// Windows cannot fsync a directory handle; NTFS journals metadata itself.
func syncDir(dir string) error { return nil }
