//go:build unit

package service

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// 上游中文错误 body 超长时，截断不得切断多字节字符（PostgreSQL 拒收非法 UTF-8，整批历史写入会失败）。
func TestTruncateForErrorBody_KeepsUTF8Boundary(t *testing.T) {
	const ellipsis = "...(body truncated)"
	cutoff := monitorErrorBodySnippetMaxBytes - len(ellipsis)
	for pad := 0; pad < 3; pad++ {
		// 前缀 ASCII 长度变化让切点轮流落在汉字的第 1/2/3 个字节上。
		body := strings.Repeat("a", pad) + strings.Repeat("网关调度受限", 40)
		got := truncateForErrorBody(body)
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d: result is not valid UTF-8: %q", pad, got)
		}
		if !strings.HasSuffix(got, ellipsis) {
			t.Fatalf("pad=%d: missing ellipsis: %q", pad, got)
		}
		if n := len(got) - len(ellipsis); n > cutoff || n < cutoff-utf8.UTFMax+1 {
			t.Fatalf("pad=%d: kept %d bytes, want within (%d-3, %d]", pad, n, cutoff, cutoff)
		}
	}
}

func TestTruncateMessage_KeepsUTF8Boundary(t *testing.T) {
	const ellipsis = "...(truncated)"
	for pad := 0; pad < 3; pad++ {
		msg := fmt.Sprintf("upstream HTTP 503: %s%s", strings.Repeat("a", pad), strings.Repeat("网关诊断", 60))
		got := truncateMessage(msg)
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d: result is not valid UTF-8: %q", pad, got)
		}
		if !strings.HasSuffix(got, ellipsis) || len(got) > monitorMessageMaxBytes {
			t.Fatalf("pad=%d: unexpected result (len=%d): %q", pad, len(got), got)
		}
	}
}

// 上游返回非 UTF-8 编码（如 GBK 错误页）时，非法字节替换为 U+FFFD，结果仍可入库。
func TestTruncateHelpers_ReplaceInvalidUTF8(t *testing.T) {
	invalid := "upstream HTTP 502: \xcd\xf8\xb9\xd8 error"
	for name, got := range map[string]string{
		"truncateMessage":      truncateMessage(invalid),
		"truncateForErrorBody": truncateForErrorBody(invalid),
	} {
		if !utf8.ValidString(got) {
			t.Fatalf("%s: result is not valid UTF-8: %q", name, got)
		}
		if !strings.Contains(got, "�") || !strings.HasSuffix(got, " error") {
			t.Fatalf("%s: unexpected result: %q", name, got)
		}
	}
	if got := truncateMessage("slow response: 1234ms"); got != "slow response: 1234ms" {
		t.Fatalf("short ASCII message changed: %q", got)
	}
}
