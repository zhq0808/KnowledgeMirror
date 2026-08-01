// Package store 负责基础设施连接：PostgreSQL 连接池（pgxpool 原生池）、Redis 客户端。
// 只管"建连 + 探活 + 池参数"，具体的读写逻辑（repository）后续在本包内实现。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"KnowledgeMirror/internal/config"
)

// pingTimeout 是建连后探活的超时，尽早暴露配置/网络问题，不拖慢启动。
const pingTimeout = 5 * time.Second

// NewPostgres 打开 PostgreSQL 连接池（pgxpool 原生池）并探活。
//
// 相比 database/sql + pgx stdlib，pgxpool 是 pgx 的原生连接池：
// 直接走 PostgreSQL 二进制协议、支持 pgx 全部特性（LISTEN/NOTIFY、COPY、批量等），
// 少一层 database/sql 抽象，性能更好。返回 *pgxpool.Pool，本身并发安全，可跨 goroutine 共享。
func NewPostgres(cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("解析 PostgreSQL DSN 失败: %w", err)
	}

	// 连接池参数：控制并发连接数与连接寿命，防止连接泄漏或被服务端 idle 超时掐断。
	poolCfg.MaxConns = int32(cfg.MaxOpenConns)                                 // 池上限
	poolCfg.MinConns = int32(cfg.MaxIdleConns)                                 // 常驻热连接数（预热，省首次建连延迟）
	poolCfg.MaxConnLifetime = time.Duration(cfg.ConnMaxLifetime) * time.Second // 连接最长存活

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL 失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	return pool, nil
}

// NewRedis 构造 Redis 客户端并探活。
func NewRedis(cfg config.RedisConfig) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
	})

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	return rdb, nil
}
