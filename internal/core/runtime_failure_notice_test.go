package core

import (
	"strings"
	"testing"
)

func TestRuntimeFailureNoticeIncludesSafeActionableErrorReason(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		want  string
		avoid string
	}{
		{name: "unavailable", code: "unavailable", want: "连接被拒绝"},
		{name: "permission", code: "denied", want: "权限拒绝"},
		{name: "unknown code is sanitized", code: "unavailable\nTOKEN=PRIVATE", want: "错误代码：`internal`", avoid: "TOKEN=PRIVATE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notice := runtimeFailureNotice(RuntimeTask{Status: "failed", ErrorCode: tt.code})
			if !strings.Contains(notice, tt.want) {
				t.Fatalf("failure notice omitted actionable reason %q: %s", tt.want, notice)
			}
			if tt.avoid != "" && strings.Contains(notice, tt.avoid) {
				t.Fatalf("failure notice leaked provider text %q: %s", tt.avoid, notice)
			}
		})
	}
}
