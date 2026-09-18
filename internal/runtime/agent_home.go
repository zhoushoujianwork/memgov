package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func effectiveAgentHome(memgovHome, configured, agent string) string {
	if configured != "" {
		return filepath.Clean(configured)
	}
	if agent == "" {
		agent = "default"
	}
	return filepath.Join(memgovHome, "agent-homes", agent)
}

func prepareAgentHome(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return fmt.Errorf("invalid Agent home")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create Agent home: %w", err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("secure Agent home: %w", err)
	}
	claudePath := filepath.Join(path, "CLAUDE.md")
	info, err := os.Lstat(claudePath)
	if os.IsNotExist(err) {
		if err := os.WriteFile(claudePath, []byte("# Agent daily notes\n\nRecord only durable, non-secret handling notes for this Agent.\n"), 0600); err != nil {
			return fmt.Errorf("create CLAUDE.md: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect CLAUDE.md: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("CLAUDE.md must be a regular file")
	}
	return os.Chmod(claudePath, 0600)
}
