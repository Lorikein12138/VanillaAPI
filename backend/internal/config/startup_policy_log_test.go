//go:build unit

package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// startupURLPolicyLogRecord 是 slog JSON 输出中 security.url_allowlist.* 记录的最小视图。
type startupURLPolicyLogRecord struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Enabled    *bool  `json:"enabled"`
	Validation string `json:"validation"`
}

// captureStartupURLPolicyLogs 把全局 slog 默认 handler 换成 JSON 缓冲，用例结束时恢复。
// 返回的函数读取当前缓冲中的 security.url_allowlist.* 记录；调用方不得并行运行。
func captureStartupURLPolicyLogs(t *testing.T) func() []startupURLPolicyLogRecord {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []startupURLPolicyLogRecord {
		var out []startupURLPolicyLogRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec startupURLPolicyLogRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			if strings.HasPrefix(rec.Msg, "security.url_allowlist.") {
				out = append(out, rec)
			}
		}
		return out
	}
}

func requireDisabledURLPolicyNotice(t *testing.T, records []startupURLPolicyLogRecord) {
	t.Helper()
	require.Len(t, records, 1)
	require.Equal(t, "INFO", records[0].Level)
	require.True(t, strings.HasPrefix(records[0].Msg, "security.url_allowlist.disabled"))
	require.NotNil(t, records[0].Enabled)
	require.False(t, *records[0].Enabled)
	require.Equal(t, "minimal_format", records[0].Validation)
}

func TestStartupURLPolicyDisabledLogsInfoOnLoad(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SECURITY_URL_ALLOWLIST_ENABLED", "false")
	records := captureStartupURLPolicyLogs(t)

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Security.URLAllowlist.Enabled)
	requireDisabledURLPolicyNotice(t, records())
}

func TestStartupURLPolicyDisabledLogsInfoOnBootstrap(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SECURITY_URL_ALLOWLIST_ENABLED", "false")
	records := captureStartupURLPolicyLogs(t)

	cfg, err := LoadForBootstrap()
	require.NoError(t, err)
	require.False(t, cfg.Security.URLAllowlist.Enabled)
	requireDisabledURLPolicyNotice(t, records())
}

func TestStartupURLPolicyEnabledEmitsNoDisabledNotice(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SECURITY_URL_ALLOWLIST_ENABLED", "true")
	records := captureStartupURLPolicyLogs(t)

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Security.URLAllowlist.Enabled)
	require.Empty(t, records())

	cfg, err = LoadForBootstrap()
	require.NoError(t, err)
	require.True(t, cfg.Security.URLAllowlist.Enabled)
	require.Empty(t, records())
}

func TestStartupURLPolicyDisabledKeepsEffectiveDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SECURITY_URL_ALLOWLIST_ENABLED", "false")

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Security.URLAllowlist.Enabled)
	require.True(t, cfg.Security.URLAllowlist.AllowPrivateHosts)
	require.True(t, cfg.Security.URLAllowlist.AllowInsecureHTTP)
	require.NotEmpty(t, cfg.Security.URLAllowlist.UpstreamHosts)
}
