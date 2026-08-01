package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/store"
)

// TestAgentTurnLeaseSchemaEnforcesSingleActiveTurnPerSession 只验证 000008 迁移定义的
// turn 租约表结构本身（约束/索引），不经过任何 repository/service 代码——
// 获取/释放租约的业务逻辑是后续独立的修改点，这里先把数据库模型钉死。
func TestAgentTurnLeaseSchemaEnforcesSingleActiveTurnPerSession(t *testing.T) {
	if os.Getenv("INTERVIEW_AGENT_INTEGRATION_TEST") != "1" {
		t.Skip("set INTERVIEW_AGENT_INTEGRATION_TEST=1 to run PostgreSQL integration tests")
	}

	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pool, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := store.RunMigrations(cfg.Postgres, os.DirFS("../../migrations"), "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	const (
		userID    = "usr_turn_lease_schema_test"
		otherUser = "usr_turn_lease_schema_other"
		sessionID = "session_00000000000000000000000000000080"
	)
	ctx := context.Background()
	cleanupTurnLeaseSchemaTest(t, pool, userID, otherUser)
	defer cleanupTurnLeaseSchemaTest(t, pool, userID, otherUser)

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0), ($2, 0, 0)`, userID, otherUser); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2)`, sessionID, userID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	future := time.Now().Add(time.Minute)
	firstMessageID := "00000000-0000-4000-8000-000000000080"
	secondMessageID := "00000000-0000-4000-8000-000000000081"

	// 1. 首次获取 active 租约，成功。
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'active', $4)`, sessionID, userID, firstMessageID, future); err != nil {
		t.Fatalf("insert first active lease: %v", err)
	}

	// 2. 同一 Session、不同 client_message_id 再抢 active 租约必须被拒绝——
	//    验证"一个 Session 只有一个活跃 turn"由数据库唯一约束保证，不依赖进程内锁。
	_, err = pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'active', $4)`, sessionID, userID, secondMessageID, future)
	if !isUniqueViolationForTest(err) {
		t.Fatalf("second active lease insert error = %v, want unique violation", err)
	}

	// 3. 把首条租约标记为 failed 后，同一 Session 才能再次获取新的 active 租约。
	if _, err := pool.Exec(ctx, `
		UPDATE agent_turn_lease SET status = 'failed'
		WHERE session_id = $1 AND client_message_id = $2`, sessionID, firstMessageID); err != nil {
		t.Fatalf("release first lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'active', $4)`, sessionID, userID, secondMessageID, future); err != nil {
		t.Fatalf("insert second active lease after release: %v", err)
	}

	// 4. 同一 (session_id, client_message_id) 不能新建第二行，即使旧行已不是 active——
	//    保证同一用户消息触发的 turn 重试命中同一条记录，用于结果恢复协议。
	_, err = pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'active', $4)`, sessionID, userID, firstMessageID, future)
	if !isUniqueViolationForTest(err) {
		t.Fatalf("duplicate client_message_id insert error = %v, want unique violation", err)
	}

	// 5. status 只能是 active/completed/failed。
	_, err = pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'bogus', $4)`, sessionID, userID, "00000000-0000-4000-8000-000000000082", future)
	if !isCheckViolationForTest(err) {
		t.Fatalf("invalid status insert error = %v, want check violation", err)
	}

	// 6. lease_expires_at 不能为空。
	_, err = pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'active', NULL)`, sessionID, userID, "00000000-0000-4000-8000-000000000083")
	if !isNotNullViolationForTest(err) {
		t.Fatalf("null lease_expires_at insert error = %v, want not-null violation", err)
	}

	// 7. (session_id, user_id) 必须真实存在于 agent_memory_session，防止租约挂到别人的会话上。
	//    用 failed 状态插入，避免和已存在的 active 租约先撞上 uk_turn_lease_active_session，
	//    确保这里验证到的确实是外键约束而不是别的唯一约束。
	_, err = pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at)
		VALUES ($1, $2, $3, 'failed', $4)`, sessionID, otherUser, "00000000-0000-4000-8000-000000000084", future)
	if !isForeignKeyViolationForTest(err) {
		t.Fatalf("mismatched owner insert error = %v, want foreign key violation", err)
	}
}

func isUniqueViolationForTest(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

func isCheckViolationForTest(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23514"
}

func isNotNullViolationForTest(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23502"
}

func isForeignKeyViolationForTest(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23503"
}

func cleanupTurnLeaseSchemaTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, userID := range userIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM agent_turn_lease WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup turn leases: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM agent_memory_episodic WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup messages: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM agent_memory_session WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup sessions: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM agent_user WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup user: %v", err)
		}
	}
}
