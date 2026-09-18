//go:build windows

package processtree

import (
	"fmt"
	"os/exec"
	"time"
)

func Fingerprint(pid int) (string, error) {
	return "", fmt.Errorf("durable process ownership requires Unix")
}
func Alive(pid int, stamp string) bool { return false }
func Reap(pid int, stamp string) error { return fmt.Errorf("process tree recovery requires Unix") }

// Windows remains unsupported for the managed Cyber service. Bound pipe waits
// on ordinary CLI calls; do not advertise process-tree isolation on Windows.
func Bind(cmd *exec.Cmd) func() { cmd.WaitDelay = 5 * time.Second; return func() {} }
