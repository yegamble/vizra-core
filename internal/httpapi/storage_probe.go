package httpapi

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
)

// The storage-root readiness check (VZ-FOUND-005).
//
// The root is the default `local` storage location: the directory uploads are
// written under. The check proves what a first upload needs — the root exists,
// is a directory, and this process can create a file in it — by creating and
// removing one empty probe file. Writability is part of the check because the
// default location can never be read-only (storage_locations_default_writable),
// and a root owned by the wrong uid would otherwise surface as EACCES on the
// first upload instead of at the probe.
//
// It is bounded: one stat, one create, one remove; no directory walk.
//
// Errors are fixed sentinels. An *os.PathError carries the path, and neither
// the readiness body nor a log line may carry it.

var (
	errStorageUnconfigured = errors.New("no storage root is configured")
	errStorageMissing      = errors.New("the storage root does not exist or cannot be read")
	errStorageNotDir       = errors.New("the storage root is not a directory")
	errStorageNotWritable  = errors.New("the storage root is not writable")
	errStorageTimeout      = errors.New("the storage root did not answer in time")
)

func checkStorageRoot(root string) error {
	if root == "" {
		return errStorageUnconfigured
	}
	info, err := os.Stat(root)
	if err != nil {
		return errStorageMissing
	}
	if !info.IsDir() {
		return errStorageNotDir
	}
	f, err := os.CreateTemp(root, ".vizra-readyz-*")
	if err != nil {
		return errStorageNotWritable
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return errStorageNotWritable
	}
	return nil
}

// storageProbe puts a deadline on a check that cannot take one. Filesystem
// calls are not cancellable, and a hung network mount would otherwise hold the
// single-flight readiness computation — and every probe behind it — forever.
// While one check is still stuck no second one is started, so a hung mount
// costs one goroutine, not one per probe.
type storageProbe struct {
	check func() error
	busy  atomic.Bool
}

func (p *storageProbe) Check(ctx context.Context) error {
	if !p.busy.CompareAndSwap(false, true) {
		return errStorageTimeout
	}
	done := make(chan error, 1)
	go func() {
		err := p.check()
		// Clear busy before delivering the result: a caller that returns on
		// this result and probes again must find the probe free (F-1).
		p.busy.Store(false)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errStorageTimeout
	}
}
