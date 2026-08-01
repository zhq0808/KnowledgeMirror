package store

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // 注册 pgx5:// 迁移驱动
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"KnowledgeMirror/internal/config"
)

// RunMigrations 用 golang-migrate 把数据库结构拉到最新版本。
//
// 迁移脚本通过 embed.FS 打进二进制，部署零外部依赖；出错即返回（由调用方 fail-fast）。
// dir 是 fsys 内迁移脚本所在目录（如 "migrations"）。
func RunMigrations(cfg config.PostgresConfig, fsys fs.FS, dir string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("加载迁移脚本失败: %w", err)
	}

	// golang-migrate 的 pgx/v5 驱动用 "pgx5://" 协议头，其余同标准连接串。
	migrateURL := strings.Replace(cfg.DSN(), "postgres://", "pgx5://", 1)

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("初始化迁移器失败: %w", err)
	}
	defer m.Close()

	// ErrNoChange 表示已是最新版本，不算错误。
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("执行数据库迁移失败: %w", err)
	}
	return nil
}
