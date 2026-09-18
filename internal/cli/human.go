package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func renderMemoryList(items []core.Memory) string {
	if len(items) == 0 {
		return "没有匹配的记忆"
	}
	var out strings.Builder
	w := tabwriter.NewWriter(&out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tSTATUS\tCATEGORY\tWORKSPACE\tTITLE\tID")
	for _, item := range items {
		workspace := item.WorkspaceID
		if workspace == "" {
			workspace = "global"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			item.Version, cleanCell(item.Status), cleanCell(item.Category), cleanCell(workspace), cleanCell(item.Title), item.ID)
	}
	_ = w.Flush()
	return strings.TrimRight(out.String(), "\n")
}

func cleanCell(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}
