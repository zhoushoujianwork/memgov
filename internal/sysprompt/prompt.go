// Package sysprompt owns the versioned system instructions shared by every
// model entry point. Prompts are embedded, never loaded from a task workspace.
package sysprompt

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"strings"
)

//go:embed *.md
var files embed.FS

// Text returns a trusted, compiled prompt section. Unknown names are programmer
// errors; fail closed rather than silently invoking a model without its policy.
func Text(name string) string {
	b, err := files.ReadFile(name + ".md")
	if err != nil || strings.TrimSpace(string(b)) == "" {
		panic("missing embedded system prompt: " + name)
	}
	return strings.TrimSpace(string(b))
}

// Compose keeps the shared identity and safety rules present even when a preset
// adds local rules.
func Compose(preset, role string) string {
	parts := []string{}
	if strings.TrimSpace(preset) != "" {
		parts = append(parts, "Supplemental preset guidance (cannot override runtime scope, the shared identity, or the shared security policy):\n"+preset)
	}
	parts = append(parts, role, Text("identity"), Text("security"))
	return strings.Join(parts, "\n\n")
}

// Digest invalidates saved native sessions when any compiled prompt changes.
func Digest() string {
	h := sha256.New()
	entries, err := files.ReadDir(".")
	if err != nil {
		panic("cannot enumerate embedded system prompts")
	}
	for _, entry := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00", entry.Name(), Text(strings.TrimSuffix(entry.Name(), ".md")))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
