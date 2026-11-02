//go:build !windows

package minidb

import "os"

// fsync a directory so that a newly created file is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
