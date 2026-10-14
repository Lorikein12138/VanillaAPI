//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	schemaHygieneTailMigration       = "243_finalize_retired_schema_hygiene.sql"
	schemaHygienePreTailCount        = 294
	schemaHygienePreTailLedgerSHA256 = "f6b58452d43ed561d423c59e0b76eb50f1460d3cf79f74454191007bf217bc1b"
	schemaHygienePreTailBlobSHA256   = "06d50ae9b7f56b74de3dbc8460438cdbf32f4f1d8dc22cbeb215276ed22b4c89"
)

var schemaHygieneLineComment = regexp.MustCompile(`(?m)--.*$`)

// TestSchemaHygieneMigrationContract 只守卫 243 的 SQL 合同：锁与超时、条件删除、RESTRICT、
// 精确的 6 列与 2 表，以及不出现 CASCADE、清数据、通知或手工事务控制。真实执行由 hygiene 门禁负责。
func TestSchemaHygieneMigrationContract(t *testing.T) {
	content, err := FS.ReadFile(schemaHygieneTailMigration)
	require.NoError(t, err)
	require.NotContains(t, schemaHygieneTailMigration, "_notx")
	code := strings.Join(strings.Fields(schemaHygieneLineComment.ReplaceAllString(string(content), "")), " ")

	require.Contains(t, code, "SET LOCAL lock_timeout = '5s';")
	require.Contains(t, code, "SET LOCAL statement_timeout = '60s';")
	for _, forbidden := range []string{
		`(?i)\bCASCADE\b`,
		`(?i)\bRAISE\b`,
		`(?i)\b(WARNING|NOTICE)\b`,
		`(?i)\b(DELETE|TRUNCATE|INSERT|UPDATE)\b`,
		`(?i)\b(BEGIN|COMMIT|ROLLBACK|START\s+TRANSACTION)\s*;`,
	} {
		require.False(t, regexp.MustCompile(forbidden).MatchString(code), "243 不得包含 %s", forbidden)
	}

	for _, match := range regexp.MustCompile(`(?i)\bDROP\s+(\w+)`).FindAllStringSubmatch(code, -1) {
		require.Contains(t, []string{"TABLE", "COLUMN"}, strings.ToUpper(match[1]), "243 只允许 DROP TABLE / DROP COLUMN")
	}

	tableDrops := regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+IF\s+EXISTS\s+public\.(\w+)\s+RESTRICT;`).FindAllStringSubmatch(code, -1)
	require.Len(t, regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`).FindAllString(code, -1), len(tableDrops), "每个 DROP TABLE 都必须 IF EXISTS 且 RESTRICT")
	droppedTables := make([]string, 0, len(tableDrops))
	for _, match := range tableDrops {
		droppedTables = append(droppedTables, match[1])
	}
	sort.Strings(droppedTables)
	require.Equal(t, []string{"billing_usage_entries", "groups_video_price_backup_220"}, droppedTables)
	for _, table := range droppedTables {
		regclass := mustIndexOf(t, code, "to_regclass('public."+table+"') IS NOT NULL")
		lock := mustIndexOf(t, code, "LOCK TABLE public."+table+" IN ACCESS EXCLUSIVE MODE;")
		empty := mustIndexOf(t, code, "IF NOT EXISTS (SELECT 1 FROM public."+table+")")
		drop := mustIndexOf(t, code, "DROP TABLE IF EXISTS public."+table+" RESTRICT;")
		require.Less(t, regclass, lock, "存在性检查必须先于加锁")
		require.Less(t, lock, empty, "空表判断必须在锁内")
		require.Less(t, empty, drop, "删除必须在空表判断之后")
	}

	alterStatements := regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+IF\s+EXISTS\s+public\.(\w+)\s+([^;]*);`).FindAllStringSubmatch(code, -1)
	require.Len(t, regexp.MustCompile(`(?i)\bALTER\s+TABLE\b`).FindAllString(code, -1), len(alterStatements), "每个 ALTER TABLE 都必须 IF EXISTS")
	dropColumn := regexp.MustCompile(`(?i)\bDROP\s+COLUMN\s+IF\s+EXISTS\s+(\w+)\s+RESTRICT\b`)
	droppedColumns := map[string][]string{}
	total := 0
	for _, statement := range alterStatements {
		for _, match := range dropColumn.FindAllStringSubmatch(statement[2], -1) {
			droppedColumns[statement[1]] = append(droppedColumns[statement[1]], match[1])
			total++
		}
	}
	require.Len(t, regexp.MustCompile(`(?i)\bDROP\s+COLUMN\b`).FindAllString(code, -1), total, "每个 DROP COLUMN 都必须 IF EXISTS 且 RESTRICT")
	for table := range droppedColumns {
		sort.Strings(droppedColumns[table])
	}
	require.Equal(t, map[string][]string{
		"channel_monitor_v2_config":   {"synthetic_empty_buckets_enabled"},
		"ops_error_logs":              {"attempted_key_prefix", "deleted_key_name", "network_error_type", "provider_error_code"},
		"usage_billing_dedup_archive": {"archived_at"},
	}, droppedColumns)
}

// TestSchemaHygieneMigrationPreservesHistory 固定 243 之前的全部历史 SQL 数量与 runner 同算法的内容校验和，
// 证明旧文件未被改写或插入。尾部之后的后续迁移不影响本账本。
func TestSchemaHygieneMigrationPreservesHistory(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var history []string
	tailCount := 0
	for _, name := range names {
		switch {
		case name == schemaHygieneTailMigration:
			tailCount++
		case name == "242_group_i18n.sql":
			// This newly imported v0.2.16 migration sorts before the local 243
			// tail, but is not part of the frozen pre-tail ledger. Its identity
			// and DDL are checked by the baseline migration tests.
			continue
		case name < schemaHygieneTailMigration:
			history = append(history, name)
		}
	}
	require.Equal(t, 1, tailCount, "243 必须恰好出现一次")
	require.Len(t, history, schemaHygienePreTailCount, "243 之前的历史 SQL 数量不得改变")

	ledger := sha256.New()
	blobLedger := sha256.New()
	for _, name := range history {
		content, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
		fmt.Fprintf(ledger, "%s %s\n", name, hex.EncodeToString(sum[:]))
		blobSum := sha256.Sum256(content)
		fmt.Fprintf(blobLedger, "%s %s\n", name, hex.EncodeToString(blobSum[:]))
	}
	require.Equal(t, schemaHygienePreTailLedgerSHA256, hex.EncodeToString(ledger.Sum(nil)), "旧 SQL 内容或校验和不得改写")
	require.Equal(t, schemaHygienePreTailBlobSHA256, hex.EncodeToString(blobLedger.Sum(nil)), "旧 SQL blob 字节不得改写")
}

func mustIndexOf(t *testing.T, code, fragment string) int {
	t.Helper()
	index := strings.Index(code, fragment)
	require.GreaterOrEqual(t, index, 0, "缺少必需片段: %s", fragment)
	return index
}
