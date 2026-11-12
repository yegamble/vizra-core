package main

import (
	"os"
	"syscall"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/doctor"
)

// diskCheck measures free space under the storage root, or the working
// directory when the root is not visible from where doctor runs.
func diskCheck(cfg *config.Config) doctor.Result {
	path := cfg.StorageRoot
	if _, err := os.Stat(path); err != nil {
		path = "."
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return doctor.CheckDisk(path, 0, err)
	}
	return doctor.CheckDisk(path, uint64(st.Bavail)*uint64(st.Bsize), nil) //nolint:gosec // Bsize is positive
}
