package migration_test

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/uvwt/nexusdock/internal/core"
)

// openControlDB 打开与生产相同配置（单连接、rollback journal、外键开启）的独立控制库。
func openControlDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := core.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "nexus.db"), 1)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// buildLegacyDatabase 用历史 DDL 建库并填充样例数据，模拟一个尚未版本化的生产库。
func buildLegacyDatabase(t *testing.T, ddl []string) *sql.DB {
	t.Helper()
	db := openControlDB(t)
	for _, statement := range ddl {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("apply legacy ddl: %v", err)
		}
	}
	seedSampleData(t, db)
	return db
}

// sampleInserts 为每张核心表准备一条代表性数据；表在历史 Schema 中不存在时跳过，
// 让同一份样例数据可以驱动 v0.1.0 / v0.2.0 / 未版本化三种 fixture。
// 插入顺序遵循外键依赖：users、oauth_clients、oauth_grants、agentdock_devices
// 和 agentdock_published_tool_contracts 在各自的依赖表之前。
var sampleInserts = []struct {
	table string
	sql   string
	args  []any
}{
	{"users",
		`INSERT INTO users (id, username, display_name, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		[]any{"user-1", "admin", "管理员", "active", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}},
	{"auth_tokens",
		`INSERT INTO auth_tokens (id, subject_type, subject_id, token_kind, token_hash, scopes_json, issued_at, expires_at, revoked_at, revoked_by_type, revoked_by_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"token-1", "user", "user-1", "session", "hash-session-token", `["admin"]`, "2026-01-01T00:00:00Z", nil, nil, nil, nil}},
	{"audit_events",
		`INSERT INTO audit_events (id, occurred_at, actor_type, actor_id, action, object_type, object_id, result, risk, approval, run_id, request_id, metadata_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"event-1", "2026-01-01T00:00:00Z", "user", "user-1", "login", "user", "user-1", "success", "low", "not_required", nil, "req-1", "{}"}},
	{"user_credentials",
		`INSERT INTO user_credentials (user_id, password_hash, password_algorithm, must_change_password, password_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		[]any{"user-1", "argon2id$secret-hash", "argon2id", 0, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}},
	{"user_sessions",
		`INSERT INTO user_sessions (id, user_id, token_hash, csrf_salt, remember_me, ip_prefix, user_agent_summary, created_at, last_seen_at, idle_expires_at, absolute_expires_at, revoked_at, revoke_reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"session-1", "user-1", "hash-session-token", "csrf-salt", 0, "127.0.0.", "macOS Chrome", "2026-01-01T00:00:00Z", "2026-01-01T00:05:00Z", "2026-01-01T01:00:00Z", "2026-02-01T00:00:00Z", nil, nil}},
	{"oauth_clients",
		`INSERT INTO oauth_clients (id, client_name, redirect_uris_json, grant_types_json, response_types_json, token_endpoint_auth_method, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"client-1", "Nexus Console", `["http://localhost:8080/callback"]`, `["authorization_code"]`, `["code"]`, "none", "2026-01-01T00:00:00Z", "2026-01-01T00:10:00Z"}},
	{"oauth_authorization_codes",
		`INSERT INTO oauth_authorization_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, resource, scope, created_at, expires_at, used_at, grant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"code-hash-1", "client-1", "user-1", "http://localhost:8080/callback", "challenge-1", "nexus", "mcp", "2026-01-01T00:00:00Z", "2026-01-01T00:10:00Z", nil, nil}},
	{"oauth_grants",
		`INSERT INTO oauth_grants (id, client_id, user_id, resource, scope, access_token_hash, refresh_token_hash, access_expires_at, refresh_expires_at, created_at, updated_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"grant-1", "client-1", "user-1", "nexus", "mcp", "access-hash-1", "refresh-hash-1", "2026-01-01T01:00:00Z", "2026-01-08T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", nil}},
	{"oauth_refresh_token_history",
		`INSERT INTO oauth_refresh_token_history (token_hash, grant_id, expires_at) VALUES (?, ?, ?)`,
		[]any{"old-refresh-hash", "grant-1", "2026-01-07T00:00:00Z"}},
	{"login_throttles",
		`INSERT INTO login_throttles (key_type, key_value, failures, blocked_until, last_failed_at) VALUES (?, ?, ?, ?, ?)`,
		[]any{"account", "admin", 2, nil, "2026-01-01T00:00:00Z"}},
	{"agentdock_devices",
		`INSERT INTO agentdock_devices (id, device_id, name, enabled, version, protocol_version, os, arch, capabilities_json, tool_contract_hash, last_seen_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{"node-1", "dock-mini", "DockMini", 1, "1.2.0", "1", "darwin", "arm64", `["tools"]`, "contract-hash-1", "2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"}},
	{"agentdock_pairing_codes",
		`INSERT INTO agentdock_pairing_codes (id, code_hash, expires_at, used_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		[]any{"pair-1", "pair-hash-1", "2026-01-01T00:15:00Z", nil, "2026-01-01T00:00:00Z"}},
	{"agentdock_tool_contracts",
		`INSERT INTO agentdock_tool_contracts (node_id, descriptors_json, updated_at) VALUES (?, ?, ?)`,
		[]any{"node-1", `[{"name":"fs.read"}]`, "2026-01-02T00:00:00Z"}},
	{"agentdock_ui_resources",
		`INSERT INTO agentdock_ui_resources (node_id, resources_json, updated_at) VALUES (?, ?, ?)`,
		[]any{"node-1", `[{"uri":"ui://dock"}]`, "2026-01-02T00:00:00Z"}},
	{"agentdock_bridge_capabilities",
		`INSERT INTO agentdock_bridge_capabilities (node_id, capabilities_json, updated_at) VALUES (?, ?, ?)`,
		[]any{"node-1", `[{"bridge":"fs"}]`, "2026-01-02T00:00:00Z"}},
	{"agentdock_published_tool_contracts",
		`INSERT INTO agentdock_published_tool_contracts (tool_name, descriptor_json, source_node_id, source_version, updated_at) VALUES (?, ?, ?, ?, ?)`,
		[]any{"fs.read", `{"name":"fs.read"}`, "node-1", "1.2.0", "2026-01-02T00:00:00Z"}},
	{"agentdock_published_tool_variants",
		`INSERT INTO agentdock_published_tool_variants (tool_name, semantic_hash) VALUES (?, ?)`,
		[]any{"fs.read", "semantic-1"}},
	{"mcp_settings",
		`INSERT INTO mcp_settings (singleton_id, mcp_apps_enabled, updated_at) VALUES (?, ?, ?)`,
		[]any{1, 1, "2026-01-01T00:00:00Z"}},
	{"runtime_ai_settings",
		`INSERT INTO runtime_ai_settings (singleton_id, embedding_enabled, embedding_endpoint, embedding_model, embedding_timeout_seconds, stage3_enabled, stage3_endpoint, stage3_model, stage3_timeout_seconds, stage3_interval_minutes, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{1, 1, "http://127.0.0.1:11434", "bge-m3", 30, 1, "http://127.0.0.1:8000", "qwen3", 60, 1440, "2026-01-01T00:00:00Z"}},
	{"runtime_ai_setting_secrets",
		`INSERT INTO runtime_ai_setting_secrets (name, ciphertext, updated_at) VALUES (?, ?, ?)`,
		[]any{"embedding_api_key", []byte{0x01, 0x02, 0x03, 0x04}, "2026-01-01T00:00:00Z"}},
}

func seedSampleData(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, sample := range sampleInserts {
		if !tableExists(t, db, sample.table) {
			continue
		}
		if _, err := db.ExecContext(t.Context(), sample.sql, sample.args...); err != nil {
			t.Fatalf("seed %s: %v", sample.table, err)
		}
	}
}

// seedLegacyTables 建几张更早时期遗留的表并各写一行，验证迁移对历史表的一次性清理。
func seedLegacyTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT NOT NULL)`,
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`,
		`CREATE TABLE devices (id TEXT PRIMARY KEY)`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO tasks (id, title) VALUES ('task-1', '旧任务')`,
		`INSERT INTO schema_migrations (version) VALUES (1)`,
		`INSERT INTO devices (id) VALUES ('device-1')`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("seed legacy table: %v", err)
		}
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found string
	err := db.QueryRowContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err == nil {
		return true
	}
	if err == sql.ErrNoRows {
		return false
	}
	t.Fatalf("check table %s: %v", name, err)
	return false
}

func listTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryColumn(t, db,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
}

// schemaObjects 返回全部显式创建的表/索引/触发器（sql 非空，排除 SQLite 自动索引）。
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryColumn(t, db,
		`SELECT type || '|' || name FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type, name`)
}

// tableColumns 返回一张表的列定义签名，用于对比升级库与全新库的列是否一致。
func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	// 表名来自同一库的 sqlite_master，不是外部输入，可以拼接进 PRAGMA 表函数。
	return queryColumn(t, db,
		`SELECT "name" || '|' || "type" || '|' || "notnull" || '|' || IFNULL("dflt_value", 'null') || '|' || "pk" FROM pragma_table_info('`+table+`')`)
}

func queryColumn(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

// dumpTableRows 把每张表的数据序列化成可比较的快照，用于断言迁移不丢不改数据。
func dumpTableRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	for _, table := range listTables(t, db) {
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatalf("select %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			parts := make([]string, len(values))
			for i, value := range values {
				parts[i] = serializeValue(value)
			}
			lines = append(lines, strings.Join(parts, "\x1f"))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		slices.Sort(lines)
		snapshot[table] = strings.Join(lines, "\n")
	}
	return snapshot
}

// serializeValue 带类型前缀序列化，避免 nil/空串、整数/文本在比较时混淆。
func serializeValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case []byte:
		return "blob:" + hex.EncodeToString(v)
	default:
		return fmt.Sprintf("%T:%v", value, value)
	}
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

func assertTablesPresent(t *testing.T, db *sql.DB, want ...string) {
	t.Helper()
	tables := listTables(t, db)
	for _, table := range want {
		if !slices.Contains(tables, table) {
			t.Fatalf("缺少表 %s，实际表: %v", table, tables)
		}
	}
}

// assertUpgradedToCurrent 断言升级完成后版本到位、历史数据逐表保全；
// dropped 中的表是迁移明确要求一次性删除的历史遗留表，不参与数据比较。
func assertUpgradedToCurrent(t *testing.T, db *sql.DB, before map[string]string, dropped ...string) {
	t.Helper()
	if got := schemaVersion(t, db); got != core.CurrentSchemaVersion {
		t.Fatalf("升级后 user_version = %d, want %d", got, core.CurrentSchemaVersion)
	}
	after := dumpTableRows(t, db)
	for _, table := range dropped {
		if _, ok := after[table]; ok {
			t.Fatalf("历史遗留表 %s 应在迁移中删除，仍然存在", table)
		}
	}
	for table, want := range before {
		if slices.Contains(dropped, table) {
			continue
		}
		got, ok := after[table]
		if !ok {
			t.Fatalf("升级后表 %s 丢失", table)
		}
		if got != want {
			t.Fatalf("升级改变了表 %s 的数据:\n升级前:\n%s\n升级后:\n%s", table, want, got)
		}
	}
}

// 全新空库没有历史数据，直接初始化到当前结构并写入版本号。
func TestEnsureSchema_全新空库直接初始化到当前版本(t *testing.T) {
	db := openControlDB(t)

	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if got := schemaVersion(t, db); got != core.CurrentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, core.CurrentSchemaVersion)
	}
	assertTablesPresent(t, db,
		"users", "auth_tokens", "audit_events", "user_credentials", "user_sessions",
		"oauth_clients", "oauth_authorization_codes", "oauth_grants", "oauth_refresh_token_history",
		"login_throttles",
		"agentdock_devices", "agentdock_pairing_codes", "agentdock_tool_contracts",
		"agentdock_ui_resources", "agentdock_bridge_capabilities",
		"agentdock_published_tool_contracts", "agentdock_published_tool_variants",
		"mcp_settings", "runtime_ai_settings", "runtime_ai_setting_secrets",
	)
}

// v0.1.0 是最老的真实历史结构：升级要补齐后来新增的表，并保全全部历史数据。
func TestEnsureSchema_从v0_1_0真实Schema升级并保全数据(t *testing.T) {
	db := buildLegacyDatabase(t, schemaV0_1_0)
	before := dumpTableRows(t, db)

	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	assertUpgradedToCurrent(t, db, before)
	assertTablesPresent(t, db, "agentdock_bridge_capabilities", "mcp_settings")

	// 关键字段语义抽查：升级只补结构，不改业务数据。
	var username string
	if err := db.QueryRowContext(t.Context(),
		`SELECT username FROM users WHERE id = 'user-1'`).Scan(&username); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if username != "admin" {
		t.Fatalf("users.username = %q, want admin", username)
	}
	var scope string
	if err := db.QueryRowContext(t.Context(),
		`SELECT scope FROM oauth_grants WHERE id = 'grant-1'`).Scan(&scope); err != nil {
		t.Fatalf("query oauth_grants: %v", err)
	}
	if scope != "mcp" {
		t.Fatalf("oauth_grants.scope = %q, want mcp", scope)
	}
	var ciphertext []byte
	if err := db.QueryRowContext(t.Context(),
		`SELECT ciphertext FROM runtime_ai_setting_secrets WHERE name = 'embedding_api_key'`).Scan(&ciphertext); err != nil {
		t.Fatalf("query runtime_ai_setting_secrets: %v", err)
	}
	if !bytes.Equal(ciphertext, []byte{0x01, 0x02, 0x03, 0x04}) {
		t.Fatalf("runtime_ai_setting_secrets.ciphertext = %x, want 01020304", ciphertext)
	}
}

func TestEnsureSchema_从v0_2_0真实Schema升级并保全数据(t *testing.T) {
	db := buildLegacyDatabase(t, schemaV0_2_0)
	before := dumpTableRows(t, db)

	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	assertUpgradedToCurrent(t, db, before)
	assertTablesPresent(t, db, "mcp_settings")
}

// 当前生产库就是这种形态：user_version=0、表已存在、可能还留着更早时期的历史表。
func TestEnsureSchema_从当前未版本化Schema升级并清理历史遗留表(t *testing.T) {
	db := buildLegacyDatabase(t, schemaUnversioned)
	seedLegacyTables(t, db)
	before := dumpTableRows(t, db)

	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	assertUpgradedToCurrent(t, db, before, "tasks", "schema_migrations", "devices")

	// mcp_settings 样例数据语义不变。
	var mcpAppsEnabled int
	if err := db.QueryRowContext(t.Context(),
		`SELECT mcp_apps_enabled FROM mcp_settings WHERE singleton_id = 1`).Scan(&mcpAppsEnabled); err != nil {
		t.Fatalf("query mcp_settings: %v", err)
	}
	if mcpAppsEnabled != 1 {
		t.Fatalf("mcp_settings.mcp_apps_enabled = %d, want 1", mcpAppsEnabled)
	}

	// 版本化之后再启动一次必须零变化（重复启动幂等）。
	rowsAfterFirstEnsure := dumpTableRows(t, db)
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	if version := schemaVersion(t, db); version != core.CurrentSchemaVersion {
		t.Fatalf("second EnsureSchema 后 user_version = %d, want %d", version, core.CurrentSchemaVersion)
	}
	if rowsAfterSecondEnsure := dumpTableRows(t, db); !mapsEqual(rowsAfterFirstEnsure, rowsAfterSecondEnsure) {
		t.Fatal("第二次 EnsureSchema 改变了数据")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, valueA := range a {
		valueB, ok := b[key]
		if !ok || valueA != valueB {
			return false
		}
	}
	return true
}

// 连续两次 Ensure，第二次必须零变化：这是每次启动都会执行的路径。
func TestEnsureSchema_重复启动零变化(t *testing.T) {
	db := openControlDB(t)
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	seedSampleData(t, db)

	snapshot := func() string {
		parts := []string{fmt.Sprintf("version=%d", schemaVersion(t, db))}
		parts = append(parts, schemaObjects(t, db)...)
		for table, rows := range dumpTableRows(t, db) {
			parts = append(parts, table+"="+rows)
		}
		slices.Sort(parts)
		return strings.Join(parts, "\n")
	}
	before := snapshot()

	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("第二次 EnsureSchema 改变了数据库状态:\n--- 升级前 ---\n%s\n--- 升级后 ---\n%s", before, after)
	}
}

// 升级路径（从 v0.1.0 重放迁移）与全新初始化必须收敛到同一结构，
// 防止只改 currentSchema 忘加 migration、或只加 migration 忘改 currentSchema。
func TestEnsureSchema_升级后结构与全新初始化收敛一致(t *testing.T) {
	fresh := openControlDB(t)
	if err := core.EnsureSchema(t.Context(), fresh); err != nil {
		t.Fatalf("EnsureSchema on fresh db: %v", err)
	}
	upgraded := buildLegacyDatabase(t, schemaV0_1_0)
	if err := core.EnsureSchema(t.Context(), upgraded); err != nil {
		t.Fatalf("EnsureSchema on legacy db: %v", err)
	}

	freshObjects := schemaObjects(t, fresh)
	upgradedObjects := schemaObjects(t, upgraded)
	if !slices.Equal(freshObjects, upgradedObjects) {
		t.Fatalf("升级后对象集合与全新初始化不一致:\n全新: %v\n升级: %v", freshObjects, upgradedObjects)
	}
	for _, table := range listTables(t, fresh) {
		want := tableColumns(t, fresh, table)
		got := tableColumns(t, upgraded, table)
		if !slices.Equal(want, got) {
			t.Fatalf("表 %s 列定义不一致:\n全新: %v\n升级: %v", table, want, got)
		}
	}
}

// 库版本比程序新说明是旧二进制遇到新库，必须拒绝启动而不是按旧结构读写。
func TestEnsureSchema_库版本比程序新时拒绝启动(t *testing.T) {
	db := openControlDB(t)
	if err := core.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	err := core.EnsureSchema(t.Context(), db)

	if err == nil {
		t.Fatal("期望拒绝更高版本的数据库，实际成功返回")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("错误信息应包含版本号 99，实际: %v", err)
	}
}
