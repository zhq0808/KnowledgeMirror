package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/store"
)

const (
	extractUserID    = "usr_extraction_repository_test"
	extractSessionID = "session_000000000000000000000000000000b1"
	extractUserMsg1  = "bbbbbbbb-0000-4000-8000-0000000000b1"
	extractAsstMsg1  = "bbbbbbbb-0000-4000-8000-0000000000b2"
	extractUserMsg2  = "bbbbbbbb-0000-4000-8000-0000000000b3"
	extractAsstMsg2  = "bbbbbbbb-0000-4000-8000-0000000000b4"
	extractClient1   = "bbbbbbbb-0000-4000-8000-0000000000c1"
	extractClient2   = "bbbbbbbb-0000-4000-8000-0000000000c2"
)

// TestPostgresExtractionRepositoryLeaseBatchFailureScan 覆盖抽取管道的持久化能力：
// 抢租约（含未过期租约/无积压/退避拒绝）、按窗口读批次、失败退避与旧 token 晚到拒绝、补扫积压。
func TestPostgresExtractionRepositoryLeaseBatchFailureScan(t *testing.T) {
	pool := requireMemoryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	cleanupExtractionRepositoryTest(t, pool, extractUserID)
	defer cleanupExtractionRepositoryTest(t, pool, extractUserID)

	seedMemoryUserSession(t, pool, extractUserID, extractSessionID)
	insertCompletedTurn(t, pool, extractSessionID, extractUserID, extractUserMsg1, 1, extractAsstMsg1, 2, extractClient1)
	insertCompletedTurn(t, pool, extractSessionID, extractUserID, extractUserMsg2, 3, extractAsstMsg2, 4, extractClient2)

	repository := store.NewPostgresMemoryRepository(pool)

	// 1. 首次抢占：无 state 行时自动按游标 0 创建，返回窗口 (0, 4]。
	lease, acquired, err := repository.AcquireExtractionLease(ctx, extractSessionID, extractUserID, time.Minute)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	if !acquired || lease.FromResultSeq != 0 || lease.ToResultSeq != 4 || lease.LeaseToken == "" {
		t.Fatalf("acquire result = %+v acquired=%v, want from 0 to 4 with token", lease, acquired)
	}

	// 2. 租约未过期时再抢必须失败。
	if _, acquiredAgain, err := repository.AcquireExtractionLease(ctx, extractSessionID, extractUserID, time.Minute); err != nil || acquiredAgain {
		t.Fatalf("second acquire acquired=%v err=%v, want not acquired", acquiredAgain, err)
	}

	// 2.1 租约过期后必须允许接管，并换一个新 token；旧 Worker 后续提交会被 fencing 拒绝。
	if _, err := pool.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET lease_until = now() - interval '1 second'
		WHERE session_id = $1`, extractSessionID); err != nil {
		t.Fatalf("expire extraction lease: %v", err)
	}
	takenOver, acquiredAfterExpiry, err := repository.AcquireExtractionLease(ctx, extractSessionID, extractUserID, time.Minute)
	if err != nil {
		t.Fatalf("acquire expired lease: %v", err)
	}
	if !acquiredAfterExpiry || takenOver.LeaseToken == lease.LeaseToken || takenOver.FromResultSeq != lease.FromResultSeq || takenOver.ToResultSeq != lease.ToResultSeq {
		t.Fatalf("takeover=%+v acquired=%v, want same window with a new token", takenOver, acquiredAfterExpiry)
	}
	lease = takenOver

	// 3. 按窗口读批次：两个 turn，各自 user/assistant 消息，按结果 seq 升序。
	turns, err := repository.LoadExtractionBatch(ctx, extractSessionID, extractUserID, lease.FromResultSeq, lease.ToResultSeq)
	if err != nil {
		t.Fatalf("load batch: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("batch turns = %d, want 2", len(turns))
	}
	if turns[0].ResultSeq != 2 || turns[0].UserMessage.MessageID != extractUserMsg1 || turns[0].AssistantMessage.MessageID != extractAsstMsg1 {
		t.Fatalf("first turn = %+v, want user %s assistant %s", turns[0], extractUserMsg1, extractAsstMsg1)
	}
	if turns[1].ResultSeq != 4 || turns[1].UserMessage.Role != "user" || turns[1].AssistantMessage.Role != "assistant" {
		t.Fatalf("second turn = %+v, want roles user/assistant", turns[1])
	}

	// 4. 失败退避：清空租约、累加失败次数、设置 next_retry_at 和 last_error_code。
	if err := repository.RecordExtractionFailure(ctx, extractSessionID, extractUserID, lease.LeaseToken, "timeout", time.Second, time.Minute); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	failures, nextRetry, errorCode, leaseToken := queryExtractionState(t, pool, extractSessionID)
	if failures != 1 || nextRetry == nil || !nextRetry.After(time.Now()) || errorCode != "timeout" || leaseToken != nil {
		t.Fatalf("state after failure: failures=%d next_retry=%v code=%q lease=%v", failures, nextRetry, errorCode, leaseToken)
	}

	// 5. 旧 lease token 晚到：token 不匹配时不改动状态（幂等无副作用）。
	if err := repository.RecordExtractionFailure(ctx, extractSessionID, extractUserID, "99999999-9999-4999-8999-999999999999", "should_ignore", time.Second, time.Minute); err != nil {
		t.Fatalf("record failure with stale token: %v", err)
	}
	if failuresAfter, _, codeAfter, _ := queryExtractionState(t, pool, extractSessionID); failuresAfter != 1 || codeAfter != "timeout" {
		t.Fatalf("stale-token failure mutated state: failures=%d code=%q", failuresAfter, codeAfter)
	}

	// 6. 退避期内不可抢占。
	if _, acquiredDuringBackoff, err := repository.AcquireExtractionLease(ctx, extractSessionID, extractUserID, time.Minute); err != nil || acquiredDuringBackoff {
		t.Fatalf("acquire during backoff acquired=%v err=%v, want not acquired", acquiredDuringBackoff, err)
	}

	// 7. 清掉退避后：补扫应包含该会话（有积压、无租约、无退避）。
	clearExtractionBackoff(t, pool, extractSessionID)
	backlog, err := repository.ScanExtractionBacklog(ctx, 100)
	if err != nil {
		t.Fatalf("scan backlog: %v", err)
	}
	if !slices.Contains(backlog, extractSessionID) {
		t.Fatalf("backlog = %v, want to contain %s", backlog, extractSessionID)
	}

	// 8. 游标推进到最新完成结果 seq 后：无积压，补扫排除该会话，且不可再抢占。
	advanceExtractionCursor(t, pool, extractSessionID, 4)
	backlog, err = repository.ScanExtractionBacklog(ctx, 100)
	if err != nil {
		t.Fatalf("scan backlog after catch-up: %v", err)
	}
	if slices.Contains(backlog, extractSessionID) {
		t.Fatalf("backlog = %v, want to exclude caught-up %s", backlog, extractSessionID)
	}
	if _, acquiredNoBacklog, err := repository.AcquireExtractionLease(ctx, extractSessionID, extractUserID, time.Minute); err != nil || acquiredNoBacklog {
		t.Fatalf("acquire with no backlog acquired=%v err=%v, want not acquired", acquiredNoBacklog, err)
	}
}

func insertCompletedTurn(t *testing.T, pool *pgxpool.Pool, sessionID, userID, userMsgID string, userSeq int64, assistantMsgID string, assistantSeq int64, clientMessageID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_episodic (message_id, session_id, user_id, agent_id, seq, role, status, content, client_message_id)
		VALUES ($1, $2, $3, 'interview-agent', $4, 'user', 'completed', $5, $6)`,
		userMsgID, sessionID, userID, userSeq, "user says "+userMsgID, clientMessageID); err != nil {
		t.Fatalf("insert user message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_episodic (message_id, session_id, user_id, agent_id, seq, parent_message_id, role, status, content)
		VALUES ($1, $2, $3, 'interview-agent', $4, $5, 'assistant', 'completed', $6)`,
		assistantMsgID, sessionID, userID, assistantSeq, userMsgID, "assistant says "+assistantMsgID); err != nil {
		t.Fatalf("insert assistant message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_turn_lease (session_id, user_id, client_message_id, status, lease_expires_at, attempt_no, user_message_id, result_message_id)
		VALUES ($1, $2, $3, 'completed', now(), 1, $4, $5)`,
		sessionID, userID, clientMessageID, userMsgID, assistantMsgID); err != nil {
		t.Fatalf("insert completed turn lease: %v", err)
	}
}

func queryExtractionState(t *testing.T, pool *pgxpool.Pool, sessionID string) (failures int, nextRetry *time.Time, errorCode string, leaseToken *string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var code *string
	if err := pool.QueryRow(ctx, `
		SELECT consecutive_failures, next_retry_at, last_error_code, lease_token::text
		FROM agent_memory_extraction_state
		WHERE session_id = $1`, sessionID).Scan(&failures, &nextRetry, &code, &leaseToken); err != nil {
		t.Fatalf("query extraction state: %v", err)
	}
	if code != nil {
		errorCode = *code
	}
	return failures, nextRetry, errorCode, leaseToken
}

func clearExtractionBackoff(t *testing.T, pool *pgxpool.Pool, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET lease_token = NULL, lease_until = NULL, next_retry_at = NULL, consecutive_failures = 0
		WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
}

func advanceExtractionCursor(t *testing.T, pool *pgxpool.Pool, sessionID string, cursor int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		UPDATE agent_memory_extraction_state
		SET last_extracted_result_seq = $2, lease_token = NULL, lease_until = NULL, next_retry_at = NULL
		WHERE session_id = $1`, sessionID, cursor); err != nil {
		t.Fatalf("advance cursor: %v", err)
	}
}

func cleanupExtractionRepositoryTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, userID := range userIDs {
		statements := []string{
			`DELETE FROM agent_memory_history_source WHERE user_id = $1`,
			`DELETE FROM agent_memory_history WHERE user_id = $1`,
			`DELETE FROM agent_memory_meta WHERE user_id = $1`,
			`DELETE FROM agent_memory_extraction_state WHERE user_id = $1`,
			`DELETE FROM agent_turn_lease WHERE user_id = $1`,
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
