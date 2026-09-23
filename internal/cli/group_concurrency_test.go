package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestGroupExecutionConcurrencyNormalization(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, value := range []string{"", "1", "4", "32", "0", "-1", "33", "\"4\""} {
			t.Run(fmt.Sprintf("bot=%t/value=%s", nested, value), func(t *testing.T) {
				group := "enabled: false"
				if value != "" {
					group += ", execution_concurrency: " + value
				}
				body := "applications:\n  group_mention: {" + group + "}\n"
				if nested {
					body = "applications:\n  bots:\n    app-main:\n      group_mention: {" + group + "}\n"
				}
				a := &app{}
				err := a.loadConfig([]byte(body))
				var normalized DualModeValidation
				if err == nil {
					normalized, err = NormalizeDualModeConfig(a.cfg)
				}
				invalid := value == "0" || value == "-1" || value == "33" || strings.Contains(value, "\"")
				if invalid {
					if err == nil {
						t.Fatalf("invalid concurrency %q accepted", value)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				g := normalized.Declaration.Applications.GroupMention
				if nested {
					g = normalized.Declaration.Applications.Bots["app-main"].GroupMention
				}
				want := value
				if want == "" {
					want = "4"
				}
				if g.ExecutionConcurrency == nil || fmt.Sprint(*g.ExecutionConcurrency) != want {
					t.Fatalf("concurrency = %v, want %s", g.ExecutionConcurrency, want)
				}
			})
		}
	}
}
