package cli

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/console"
	"github.com/zhoushoujianwork/memgov/internal/core"
	localservice "github.com/zhoushoujianwork/memgov/internal/service"
	"golang.org/x/sys/unix"
)

const consoleSessionEnv = "MEMGOV_SERVICE_CONSOLE_SESSION"

func (a *app) runUnified(ctx context.Context, noUI bool, port int, open, migrate, managed bool) error {
	// Discard credentials inherited from an older service binary.
	_ = os.Unsetenv(consoleSessionEnv)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	requests := make(chan console.RestartRequest, 1)
	var requested atomic.Bool
	var updated atomic.Bool
	if managed {
		initial, err := os.Stat(executable)
		if err != nil {
			return err
		}
		go func() {
			if localservice.WaitReplacement(runCtx, executable, initial, time.Second) {
				updated.Store(true)
				cancel()
			}
		}()
	}
	restart := func(requestCtx context.Context, request console.RestartRequest) (func(), error) {
		if err := requestCtx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(executable)
		if err != nil || !info.Mode().IsRegular() || unix.Access(executable, unix.X_OK) != nil {
			return nil, core.Fail("unavailable", "installed executable is unavailable; reinstall before restarting")
		}
		if !requested.CompareAndSwap(false, true) {
			return nil, core.Fail("conflict", "service restart already requested")
		}
		return func() {
			requests <- request
			cancel()
		}, nil
	}
	// This call closes the database, listener and process lock before exec.
	if err := a.runUnifiedOnce(runCtx, noUI, port, open, migrate, managed, restart); err != nil {
		return err
	}
	// Exiting hands replacement and even a failed exec back to launchd. The
	// saved definition preserves the fixed port and the installed binary path.
	if managed && updated.Load() {
		return nil
	}
	select {
	case request := <-requests:
		if ctx.Err() != nil {
			return nil
		}
		_, portString, err := net.SplitHostPort(request.Host)
		if err != nil {
			return err
		}
		if _, err := strconv.Atoi(portString); err != nil {
			return err
		}
		// Keep the original foreground process, arguments, and chosen UI port.
		args := append([]string{executable}, os.Args[1:]...)
		args = append(args, "--port", portString, "--open=false")
		return unix.Exec(executable, args, os.Environ())
	default:
		return nil
	}
}
