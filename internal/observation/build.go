package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

var buildOnce sync.Once
var executableBuild string

// Capture the running binary at startup, not after a later install replaces it.
func ExecutableBuild() string {
	buildOnce.Do(func() { executableBuild = InstalledBuild() })
	return executableBuild
}

func InstalledBuild() string {
	path, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
