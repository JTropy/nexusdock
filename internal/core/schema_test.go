package core

import (
	"path/filepath"
	"strings"
	"testing"
)

// 迁移失败必须整体回滚：失败迁移里的表结构变更、数据写入和版本号推进都不能留下，
// 库停在失败前最后一个成功迁移的版本，下次启动从那里继续。
func TestMigrateSchema_迁移失败时整体回滚(t *testing.T) {
	ctx := t.Context()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "nexus.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 构造一个停在 v2 的历史库：直接执行 v1、v2 的语句并写版本号。
	for _, migration := range schemaMigrations[:2] {
		for _, statement := range migration.statements {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("apply %s: %v", migration.name, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	// 注入一个先做了一半工作、最后一步必然失败的 v3 迁移。
	// 第一条语句故意直接推进版本号，验证版本号写入同样参与事务回滚。
	failing := schemaMigration{
		version: 3,
		name:    "注入失败步骤",
		statements: []string{
			`PRAGMA user_version = 99`,
			`CREATE TABLE migration_rollback_probe (id TEXT PRIMARY KEY)`,
			`INSERT INTO migration_rollback_probe (id) VALUES ('half')`,
			`CREATE TABLE definitely_broken (`,
		},
	}
	migrations := append(append([]schemaMigration{}, schemaMigrations[:2]...), failing)

	err = migrateSchema(ctx, db, migrations)

	if err == nil {
		t.Fatal("期望迁移失败返回错误，实际成功")
	}
	if !strings.Contains(err.Error(), "v3") || !strings.Contains(err.Error(), "注入失败步骤") {
		t.Fatalf("错误信息应包含版本号和迁移名称定位，实际: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("user_version = %d, want 2（失败迁移的版本号推进必须回滚）", version)
	}
	var probeCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'migration_rollback_probe'`).Scan(&probeCount); err != nil {
		t.Fatal(err)
	}
	if probeCount != 0 {
		t.Fatal("失败迁移的半成品表 migration_rollback_probe 不应存在")
	}
	// 迁移前已有的结构必须原样保留。
	var usersCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'users'`).Scan(&usersCount); err != nil {
		t.Fatal(err)
	}
	if usersCount != 1 {
		t.Fatal("回滚不应影响迁移前已存在的表")
	}
}
