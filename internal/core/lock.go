package core

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"time"
)

// All memgov processes hold a shared file lock for the lifetime of their DB
// connection. Restore and physical cleanup require exclusive ownership.
func lockFile(ctx context.Context, path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, dbError(ctx.Err())
		case <-deadline.C:
			f.Close()
			return nil, Fail("unavailable", "database maintenance lock is busy; retry later")
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return errors.Join(err, f.Close())
}
func OpenMaintenance(ctx context.Context, path string) (*Store, error) {
	return openStore(ctx, path, false, true, false)
}
