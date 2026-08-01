package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
)

const (
	memoryUserID       = "usr_memory_repository_test"
	memoryOtherUserID  = "usr_memory_repository_other"
	memorySessionID    = "session_000000000000000000000000000000a1"
	memoryOtherSession = "session_000000000000000000000000000000a2"
	memoryMessageA     = "aaaaaaaa-0000-4000-8000-0000000000a1"
	memoryMessageB     = "aaaaaaaa-0000-4000-8000-0000000000a2"
	memoryMessageC     = "aaaaaaaa-0000-4000-8000-0000000000a3"
	memoryLease1       = "11111111-1111-4111-8111-111111111111"
	memoryLease2       = "22222222-2222-4222-8222-222222222222"
	memoryLease3       = "33333333-3333-4333-8333-333333333333"
	memoryLease4       = "44444444-4444-4444-8444-444444444444"
)

func confidence(value float64) *float64 { return &value }

// TestPostgresMemoryRepositoryAppliesAddUpdateDeleteLifecycle 覆盖 ADD/UPDATE/DELETE 全流程、
// 历史与来源追加、乐观锁版本冲突、游标/租约条件提交，以及软删除后召回不再返回该记忆。
func TestPostgresMemoryRepositoryAppliesAddUpdateDeleteLifecycle(t *testing.T) {
	pool := requireMemoryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	cleanupMemoryRepositoryTest(t, pool, memoryUserID, memoryOtherUserID)
	defer cleanupMemoryRepositoryTest(t, pool, memoryUserID, memoryOtherUserID)

	seedMemoryUserSession(t, pool, memoryUserID, memorySessionID)
	insertMemorySourceMessage(t, pool, memorySessionID, memoryUserID, memoryMessageA, 1, "我一直不喜欢甜食")
	insertMemorySourceMessage(t, pool, memorySessionID, memoryUserID, memoryMessageB, 3, "我现在可以吃一点微辣了")
	insertExtractionState(t, pool, memorySessionID, memoryUserID, memoryLease1, 0)

	repository := store.NewPostgresMemoryRepository(pool)

	// 1. ADD：写入 version 1 + 0->1 历史 + 来源，游标 0->2，租约清空。
	addResult, err := repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID:           memoryUserID,
		AgentID:          service.MemoryAgentID,
		SessionID:        memorySessionID,
		ExtractorModel:   "deepseek-chat",
		ExtractorVersion: "memory-extractor-v1",
		FromResultSeq:    0,
		ToResultSeq:      2,
		LeaseToken:       memoryLease1,
		Operations: []service.ResolvedOperation{{
			Action:                service.MemoryActionAdd,
			MemoryType:            "preference",
			MemoryValue:           "用户避免甜食",
			Confidence:            confidence(0.92),
			LatestSourceMessageID: memoryMessageA,
			Sources:               []service.ResolvedSource{{SourceOrder: 1, MessageID: memoryMessageA, EvidenceQuote: "不喜欢甜食"}},
		}},
	})
	if err != nil {
		t.Fatalf("apply ADD: %v", err)
	}
	if addResult.Added != 1 || addResult.ToResultSeq != 2 {
		t.Fatalf("add result = %+v, want Added 1 ToResultSeq 2", addResult)
	}

	memoryID, version, value, latest, deleted := queryCurrentMemory(t, pool, memoryUserID)
	if version != 1 || value != "用户避免甜食" || latest != memoryMessageA || deleted {
		t.Fatalf("after ADD memory version=%d value=%q latest=%q deleted=%v", version, value, latest, deleted)
	}
	assertCursor(t, pool, memorySessionID, 2, true)
	assertHistory(t, pool, memoryID, service.MemoryActionAdd, 0, 1)
	if sources := countHistorySources(t, pool, memoryID); sources != 1 {
		t.Fatalf("ADD history sources = %d, want 1", sources)
	}

	// 2. UPDATE：乐观锁 version 1，写 1->2 历史，游标 2->4，latest 换成 seq 更大的来源。
	setExtractionLease(t, pool, memorySessionID, memoryUserID, memoryLease2)
	updateResult, err := repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID:           memoryUserID,
		AgentID:          service.MemoryAgentID,
		SessionID:        memorySessionID,
		ExtractorModel:   "deepseek-chat",
		ExtractorVersion: "memory-extractor-v1",
		FromResultSeq:    2,
		ToResultSeq:      4,
		LeaseToken:       memoryLease2,
		Operations: []service.ResolvedOperation{{
			Action:                service.MemoryActionUpdate,
			MemoryID:              memoryID,
			ExpectedVersion:       1,
			MemoryType:            "preference",
			MemoryValue:           "用户可以接受少量微辣",
			Confidence:            confidence(0.88),
			LatestSourceMessageID: memoryMessageB,
			Sources:               []service.ResolvedSource{{SourceOrder: 1, MessageID: memoryMessageB, EvidenceQuote: "可以吃一点微辣"}},
		}},
	})
	if err != nil {
		t.Fatalf("apply UPDATE: %v", err)
	}
	if updateResult.Updated != 1 {
		t.Fatalf("update result = %+v, want Updated 1", updateResult)
	}
	_, version, value, latest, _ = queryCurrentMemory(t, pool, memoryUserID)
	if version != 2 || value != "用户可以接受少量微辣" || latest != memoryMessageB {
		t.Fatalf("after UPDATE version=%d value=%q latest=%q", version, value, latest)
	}
	assertCursor(t, pool, memorySessionID, 4, true)
	assertHistory(t, pool, memoryID, service.MemoryActionUpdate, 1, 2)

	// 3. 版本冲突：用过期的 ExpectedVersion=1 再次 UPDATE 必须被乐观锁拒绝，整批回滚，游标不动。
	setExtractionLease(t, pool, memorySessionID, memoryUserID, memoryLease3)
	_, err = repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID:        memoryUserID,
		AgentID:       service.MemoryAgentID,
		SessionID:     memorySessionID,
		FromResultSeq: 4,
		ToResultSeq:   6,
		LeaseToken:    memoryLease3,
		Operations: []service.ResolvedOperation{{
			Action:                service.MemoryActionUpdate,
			MemoryID:              memoryID,
			ExpectedVersion:       1,
			MemoryType:            "preference",
			MemoryValue:           "陈旧结果不应覆盖",
			Confidence:            confidence(0.9),
			LatestSourceMessageID: memoryMessageB,
			Sources:               []service.ResolvedSource{{SourceOrder: 1, MessageID: memoryMessageB, EvidenceQuote: "微辣"}},
		}},
	})
	if !errors.Is(err, service.ErrMemoryVersionConflict) {
		t.Fatalf("stale update err = %v, want ErrMemoryVersionConflict", err)
	}
	_, version, value, _, _ = queryCurrentMemory(t, pool, memoryUserID)
	if version != 2 || value != "用户可以接受少量微辣" {
		t.Fatalf("memory changed after conflict: version=%d value=%q", version, value)
	}
	assertCursor(t, pool, memorySessionID, 4, false) // lease3 仍在（本次提交回滚，未清空）

	// 4. 租约/游标条件提交：错误 lease token 必须被拒绝。
	_, err = repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID: memoryUserID, AgentID: service.MemoryAgentID, SessionID: memorySessionID,
		FromResultSeq: 4, ToResultSeq: 6, LeaseToken: "99999999-9999-4999-8999-999999999999",
	})
	if !errors.Is(err, service.ErrExtractionCursorConflict) {
		t.Fatalf("wrong lease err = %v, want ErrExtractionCursorConflict", err)
	}
	// 错误 from 游标同样被拒绝。
	_, err = repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID: memoryUserID, AgentID: service.MemoryAgentID, SessionID: memorySessionID,
		FromResultSeq: 999, ToResultSeq: 1000, LeaseToken: memoryLease3,
	})
	if !errors.Is(err, service.ErrExtractionCursorConflict) {
		t.Fatalf("stale cursor err = %v, want ErrExtractionCursorConflict", err)
	}

	// 5. DELETE：软删除当前快照 version 加 1，写 2->3 历史；召回不再返回该记忆。
	deleteResult, err := repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID:           memoryUserID,
		AgentID:          service.MemoryAgentID,
		SessionID:        memorySessionID,
		ExtractorModel:   "deepseek-chat",
		ExtractorVersion: "memory-extractor-v1",
		FromResultSeq:    4,
		ToResultSeq:      6,
		LeaseToken:       memoryLease3,
		Operations: []service.ResolvedOperation{{
			Action:                service.MemoryActionDelete,
			MemoryID:              memoryID,
			ExpectedVersion:       2,
			MemoryType:            "preference",
			Confidence:            confidence(0.95),
			LatestSourceMessageID: memoryMessageB,
			Sources:               []service.ResolvedSource{{SourceOrder: 1, MessageID: memoryMessageB, EvidenceQuote: "微辣"}},
		}},
	})
	if err != nil {
		t.Fatalf("apply DELETE: %v", err)
	}
	if deleteResult.Deleted != 1 {
		t.Fatalf("delete result = %+v, want Deleted 1", deleteResult)
	}
	_, _, _, _, deleted = queryCurrentMemoryAllowMissing(t, pool, memoryUserID)
	if !deleted {
		t.Fatalf("memory should be soft-deleted after DELETE")
	}
	assertCursor(t, pool, memorySessionID, 6, true)
	assertHistory(t, pool, memoryID, service.MemoryActionDelete, 2, 3)

	current, err := repository.ListCurrentMemories(ctx, memoryUserID, service.MemoryAgentID, 0)
	if err != nil {
		t.Fatalf("list current memories: %v", err)
	}
	if len(current) != 0 {
		t.Fatalf("current memories = %d, want 0 after soft delete", len(current))
	}
}

// TestPostgresMemoryRepositoryRejectsCrossUserSource 验证来源消息属于其他用户时，
// 复合外键 (source_message_id,user_id) 会拒绝写入，整批回滚，不留下任何记忆或历史。
func TestPostgresMemoryRepositoryRejectsCrossUserSource(t *testing.T) {
	pool := requireMemoryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	cleanupMemoryRepositoryTest(t, pool, memoryUserID, memoryOtherUserID)
	defer cleanupMemoryRepositoryTest(t, pool, memoryUserID, memoryOtherUserID)

	seedMemoryUserSession(t, pool, memoryUserID, memorySessionID)
	seedMemoryUserSession(t, pool, memoryOtherUserID, memoryOtherSession)
	// memoryMessageC 属于另一个用户。
	insertMemorySourceMessage(t, pool, memoryOtherSession, memoryOtherUserID, memoryMessageC, 1, "别人的消息")
	insertExtractionState(t, pool, memorySessionID, memoryUserID, memoryLease1, 0)

	repository := store.NewPostgresMemoryRepository(pool)
	_, err := repository.ApplyExtraction(ctx, service.ApplyExtractionRequest{
		UserID:        memoryUserID,
		AgentID:       service.MemoryAgentID,
		SessionID:     memorySessionID,
		FromResultSeq: 0,
		ToResultSeq:   2,
		LeaseToken:    memoryLease1,
		Operations: []service.ResolvedOperation{{
			Action:                service.MemoryActionAdd,
			MemoryType:            "preference",
			MemoryValue:           "越权来源应被拒绝",
			Confidence:            confidence(0.9),
			LatestSourceMessageID: memoryMessageC,
			Sources:               []service.ResolvedSource{{SourceOrder: 1, MessageID: memoryMessageC, EvidenceQuote: "别人的消息"}},
		}},
	})
	if !errors.Is(err, service.ErrMemorySourceForbidden) {
		t.Fatalf("cross-user source err = %v, want ErrMemorySourceForbidden", err)
	}

	current, err := repository.ListCurrentMemories(ctx, memoryUserID, service.MemoryAgentID, 0)
	if err != nil {
		t.Fatalf("list current memories: %v", err)
	}
	if len(current) != 0 {
		t.Fatalf("current memories = %d, want 0 (batch fully rolled back)", len(current))
	}
	assertCursor(t, pool, memorySessionID, 0, false) // 游标未推进，租约仍在（整批回滚）
}

func requireMemoryTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

func seedMemoryUserSession(t *testing.T, pool *pgxpool.Pool, userID, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status) VALUES ($1, 0, 0)`, userID); err != nil {
		t.Fatalf("insert user %s: %v", userID, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id) VALUES ($1, $2)`, sessionID, userID); err != nil {
		t.Fatalf("insert session %s: %v", sessionID, err)
	}
}

func insertMemorySourceMessage(t *testing.T, pool *pgxpool.Pool, sessionID, userID, messageID string, seq int64, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_episodic (message_id, session_id, user_id, agent_id, seq, role, status, content)
		VALUES ($1, $2, $3, 'interview-agent', $4, 'user', 'completed', $5)`,
		messageID, sessionID, userID, seq, content); err != nil {
		t.Fatalf("insert source message %s: %v", messageID, err)
	}
}

func insertExtractionState(t *testing.T, pool *pgxpool.Pool, sessionID, userID, leaseToken string, cursor int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_extraction_state (session_id, user_id, last_extracted_result_seq, lease_token, lease_until)
		VALUES ($1, $2, $3, $4, now() + interval '5 minutes')`,
		sessionID, userID, cursor, leaseToken); err != nil {
		t.Fatalf("insert extraction state: %v", err)
	}
}

func setExtractionLease(t *testing.T, pool *pgxpool.Pool, sessionID, userID, leaseToken string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET lease_token = $3, lease_until = now() + interval '5 minutes'
		WHERE session_id = $1 AND user_id = $2`, sessionID, userID, leaseToken); err != nil {
		t.Fatalf("set extraction lease: %v", err)
	}
}

func queryCurrentMemory(t *testing.T, pool *pgxpool.Pool, userID string) (memoryID string, version int64, value, latest string, deleted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
		SELECT memory_id::text, version, memory_value, COALESCE(latest_source_message_id::text, ''), deleted_at IS NOT NULL
		FROM agent_memory_meta
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT 1`, userID).Scan(&memoryID, &version, &value, &latest, &deleted); err != nil {
		t.Fatalf("query current memory: %v", err)
	}
	return memoryID, version, value, latest, deleted
}

func queryCurrentMemoryAllowMissing(t *testing.T, pool *pgxpool.Pool, userID string) (memoryID string, version int64, value, latest string, deleted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
		SELECT memory_id::text, version, memory_value, COALESCE(latest_source_message_id::text, ''), deleted_at IS NOT NULL
		FROM agent_memory_meta
		WHERE user_id = $1
		ORDER BY created_at ASC
		LIMIT 1`, userID).Scan(&memoryID, &version, &value, &latest, &deleted); err != nil {
		t.Fatalf("query current memory (allow deleted): %v", err)
	}
	return memoryID, version, value, latest, deleted
}

func assertCursor(t *testing.T, pool *pgxpool.Pool, sessionID string, wantSeq int64, wantLeaseCleared bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cursor int64
	var leaseToken *string
	if err := pool.QueryRow(ctx, `
		SELECT last_extracted_result_seq, lease_token::text
		FROM agent_memory_extraction_state
		WHERE session_id = $1`, sessionID).Scan(&cursor, &leaseToken); err != nil {
		t.Fatalf("query cursor: %v", err)
	}
	if cursor != wantSeq {
		t.Fatalf("cursor = %d, want %d", cursor, wantSeq)
	}
	if wantLeaseCleared && leaseToken != nil {
		t.Fatalf("lease token = %v, want cleared after successful commit", *leaseToken)
	}
}

func assertHistory(t *testing.T, pool *pgxpool.Pool, memoryID, action string, fromVersion, toVersion int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var gotAction string
	var gotFrom, gotTo int64
	if err := pool.QueryRow(ctx, `
		SELECT action, from_version, to_version
		FROM agent_memory_history
		WHERE memory_id = $1
		ORDER BY to_version DESC
		LIMIT 1`, memoryID).Scan(&gotAction, &gotFrom, &gotTo); err != nil {
		t.Fatalf("query history: %v", err)
	}
	if gotAction != action || gotFrom != fromVersion || gotTo != toVersion {
		t.Fatalf("latest history = %s %d->%d, want %s %d->%d", gotAction, gotFrom, gotTo, action, fromVersion, toVersion)
	}
}

func countHistorySources(t *testing.T, pool *pgxpool.Pool, memoryID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM agent_memory_history_source AS hs
		JOIN agent_memory_history AS h ON h.history_id = hs.history_id
		WHERE h.memory_id = $1`, memoryID).Scan(&count); err != nil {
		t.Fatalf("count history sources: %v", err)
	}
	return count
}

func cleanupMemoryRepositoryTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, userID := range userIDs {
		statements := []string{
			`DELETE FROM agent_memory_history_source WHERE user_id = $1`,
			`DELETE FROM agent_memory_history WHERE user_id = $1`,
			`DELETE FROM agent_memory_meta WHERE user_id = $1`,
			`DELETE FROM agent_memory_extraction_state WHERE user_id = $1`,
			`DELETE FROM agent_memory_episodic WHERE user_id = $1`,
			`DELETE FROM agent_memory_session WHERE user_id = $1`,
			`DELETE FROM agent_user WHERE user_id = $1`,
		}
		for _, statement := range statements {
			if _, err := pool.Exec(ctx, statement, userID); err != nil {
				t.Fatalf("cleanup %q for %s: %v", statement, userID, err)
			}
		}
	}
}
