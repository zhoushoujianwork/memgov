package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// prepareGroupMemoryTool exposes exactly three read operations, bound to the
// triggering conversation. It does not grant general shell or source access.
func prepareGroupMemoryTool(in ExecutionInput) (string, error) {
	conversationBound := in.ApplicationMode == "group_mention" || in.ApplicationMode == "proactive" && in.MemoryScope == "conversation_published"
	if !conversationBound || !hasAgentCapability(in.Capabilities, "memory_read") || in.Home == "" || in.ChannelID == "" || in.ConversationID == "" {
		return "", nil
	}
	binary, err := directMemgovBinary(in.Home)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(in.WorkDir, ".claude", "tools")
	for _, path := range []string{filepath.Join(in.WorkDir, ".claude"), dir} {
		if info, err := os.Lstat(path); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return "", core.Fail("denied", "group memory control directory is not a regular directory")
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, "memgov-group-memory")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return "", core.Fail("denied", "group memory control is not a regular file")
	}
	base := groupMemoryShellQuote(binary) + " --home " + groupMemoryShellQuote(in.Home)
	conversation := " --conversation " + groupMemoryShellQuote(in.ConversationID)
	channel := groupMemoryShellQuote(in.ChannelID)
	script := fmt.Sprintf(`#!/bin/sh
set -eu
deny() { echo 'Only latest [1..20], recall <query>, and show <memory-id> are allowed' >&2; exit 2; }
[ "$#" -ge 1 ] || deny
case "$1" in
 latest)
  [ "$#" -le 2 ] || deny
  count=${2:-1}
  case "$count" in 1|2|3|4|5|6|7|8|9|10|11|12|13|14|15|16|17|18|19|20) ;; *) deny ;; esac
  exec %s audience list %s%s --limit "$count"
  ;;
 recall)
  [ "$#" -eq 2 ] || deny
  [ -n "$2" ] && [ "${#2}" -le 2000 ] || deny
  case "$2" in -*) deny ;; esac
  exec %s audience recall %s "$2"%s --budget-chars 12000
  ;;
 show)
  [ "$#" -eq 2 ] || deny
  [ -n "$2" ] && [ "${#2}" -le 128 ] || deny
  case "$2" in -*) deny ;; esac
  exec %s audience show %s "$2"%s
  ;;
 *) deny ;;
esac
`, base, channel, conversation, base, channel, conversation, base, channel, conversation)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return "", err
	}
	return path, nil
}

func groupMemoryShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
