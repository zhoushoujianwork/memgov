package cli

import (
	"reflect"
	"testing"
)

func TestParseRuntimeRunnerPIDsMatchesStartAndPriorRestart(t *testing.T) {
	raw := `
101 /repo/.memgov/bin/memgov --home /tmp/home --config /tmp/config.yaml runtime start owner-private
102 /repo/.memgov/bin/memgov runtime start group-mention
103 /repo/.memgov/bin/memgov runtime restart owner-private
104 /bin/zsh -c /repo/.memgov/bin/memgov runtime start owner-private
105 /repo/.memgov/bin/memgov runtime start owner-private
bad /repo/.memgov/bin/memgov runtime start owner-private
`
	got := parseRuntimeRunnerPIDs(raw, "owner-private", 105)
	if want := []int{101, 103}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime process match = %v, want %v", got, want)
	}
}
