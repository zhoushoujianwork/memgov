package service

import (
	"context"
	"debug/elf"
	"debug/macho"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

func sameExecutable(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

// WaitReplacement ignores missing files and incomplete/invalid downloads. Two
// unchanged observations debounce replacement; installers should rename an
// already-built executable atomically, never truncate the running file.
func WaitReplacement(ctx context.Context, path string, initial os.FileInfo, interval time.Duration) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending os.FileInfo
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			current, err := os.Stat(path)
			if err != nil || !current.Mode().IsRegular() || current.Size() == 0 || sameExecutable(initial, current) || unix.Access(path, unix.X_OK) != nil {
				pending = nil
				continue
			}
			if sameExecutable(pending, current) && executableFormat(path) {
				return true
			}
			pending = current
		}
	}
}

func executableFormat(path string) bool {
	if runtime.GOOS == "darwin" {
		f, err := macho.Open(path)
		if err == nil {
			f.Close()
			return true
		}
		fat, err := macho.OpenFat(path)
		if err != nil {
			return false
		}
		fat.Close()
		return true
	}
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
