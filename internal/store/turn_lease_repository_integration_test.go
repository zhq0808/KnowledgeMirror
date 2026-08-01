package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
)

func TestPostgresTurnLeaseRepositoryAcquireAndRelease(t *testing.T) {
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
		userID     = "usr_turn_lease_repo_test"
		otherUser  = "usr_turn_lease_repo_other"
		sessionID  = "session_00000000000000000000000000000090"
		otherOwned = "session_00000000000000000000000000000091"
	)
	ctx := context.Background()
	cleanupTurnLeaseRepositoryTest(t, pool, userID, otherUser)
	defer cleanupTurnLeaseRepositoryTest(t, pool, userID, otherUser)

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0), ($2, 0, 0)`, userID, otherUser); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2), ($3, $4)`, sessionID, userID, otherOwned, otherUser); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	turnLeaseService := service.NewTurnLeaseService(store.NewPostgresTurnLeaseRepository(pool))
	firstMessageID := "00000000-0000-4000-8000-000000000090"

	// 1. 首次获取，成功持有 active 租约。
	first, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: firstMessageID,
		Content:         "first question",
		LeaseDuration:   time.Minute,
	})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !first.Acquired || first.Lease.Status != service.TurnLeaseActive {
		t.Fatalf("first acquire result = %+v, want acquired active", first)
	}

	// 2. 同一 client_message_id 在租约未过期时重试，只能得到“处理中”，不能续期或获得执行权。
	_, err = turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: firstMessageID,
		Content:         "first question",
		LeaseDuration:   time.Minute,
	})
	if !errors.Is(err, service.ErrTurnInProgress) {
		t.Fatalf("duplicate active acquire error = %v, want ErrTurnInProgress", err)
	}

	// 3. 别的 client_message_id 在租约未过期时来抢，必须冲突。
	secondMessageID := "00000000-0000-4000-8000-000000000091"
	_, err = turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: secondMessageID,
		Content:         "second question",
		LeaseDuration:   time.Minute,
	})
	if !errors.Is(err, service.ErrTurnLeaseConflict) {
		t.Fatalf("conflicting acquire error = %v, want ErrTurnLeaseConflict", err)
	}
	var rejectedMessageCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_memory_episodic
		WHERE session_id = $1 AND client_message_id = $2`, sessionID, secondMessageID).Scan(&rejectedMessageCount); err != nil {
		t.Fatalf("count rejected user message: %v", err)
	}
	if rejectedMessageCount != 0 {
		t.Fatalf("rejected user message count = %d, want 0 because acquire transaction must roll back", rejectedMessageCount)
	}

	// 4. 释放后，别的 client_message_id 才能获取新的 active 租约。
	firstReply, err := turnLeaseService.Complete(ctx, service.CompleteTurnRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: firstMessageID,
		AttemptNo:       first.Lease.AttemptNo,
		UserMessageID:   first.UserMessage.MessageID,
		Content:         "first answer",
	})
	if err != nil {
		t.Fatalf("complete first turn: %v", err)
	}
	second, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: secondMessageID,
		Content:         "second question",
		LeaseDuration:   time.Minute,
	})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if !second.Acquired || second.Lease.ID == first.Lease.ID {
		t.Fatalf("acquire after release result = %+v, want a new active lease", second)
	}

	// 5. 已终态的 client_message_id 再次获取：不重复占用，直接把历史终态原样返回。
	terminalHit, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: firstMessageID,
		Content:         "first question",
		LeaseDuration:   time.Minute,
	})
	if err != nil {
		t.Fatalf("acquire terminal client_message_id: %v", err)
	}
	if terminalHit.Acquired || terminalHit.Lease.Status != service.TurnLeaseCompleted ||
		terminalHit.Lease.ResultMessageID != firstReply.MessageID {
		t.Fatalf("terminal acquire result = %+v, want not acquired + completed", terminalHit)
	}

	// 释放掉步骤 4 留下的 active 租约，让 Session 回到空闲，才能测下面的过期抢占场景。
	if _, err := turnLeaseService.Complete(ctx, service.CompleteTurnRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: secondMessageID,
		AttemptNo:       second.Lease.AttemptNo,
		UserMessageID:   second.UserMessage.MessageID,
		Content:         "second answer",
	}); err != nil {
		t.Fatalf("complete second turn: %v", err)
	}

	// 6. 已过期但仍标记 active 的旧占用者应被判定失败并腾出位置，新 client_message_id 可以拿到租约。
	expiringMessageID := "00000000-0000-4000-8000-000000000092"
	expiring, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: expiringMessageID,
		Content:         "expiring question",
		LeaseDuration:   time.Millisecond,
	})
	if err != nil || !expiring.Acquired {
		t.Fatalf("acquire soon-to-expire lease: result=%+v err=%v", expiring, err)
	}
	time.Sleep(20 * time.Millisecond)

	reclaimMessageID := "00000000-0000-4000-8000-000000000093"
	reclaimed, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: reclaimMessageID,
		Content:         "replacement question",
		LeaseDuration:   time.Minute,
	})
	if err != nil {
		t.Fatalf("reclaim expired lease: %v", err)
	}
	if !reclaimed.Acquired || reclaimed.Lease.ClientMessageID != reclaimMessageID {
		t.Fatalf("reclaim result = %+v, want acquired for %s", reclaimed, reclaimMessageID)
	}
	var expiredStatus string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM agent_turn_lease WHERE session_id = $1 AND client_message_id = $2`,
		sessionID, expiringMessageID).Scan(&expiredStatus); err != nil {
		t.Fatalf("query reclaimed-away lease status: %v", err)
	}
	if expiredStatus != string(service.TurnLeaseFailed) {
		t.Fatalf("reclaimed-away lease status = %s, want failed", expiredStatus)
	}

	// 释放掉步骤 6 留下的 active 租约，确保下面的 FK 校验测的是"用户不归属该会话"，
	// 而不是先撞上"会话已被占用"的冲突分支。
	if _, err := turnLeaseService.Complete(ctx, service.CompleteTurnRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: reclaimMessageID,
		AttemptNo:       reclaimed.Lease.AttemptNo,
		UserMessageID:   reclaimed.UserMessage.MessageID,
		Content:         "replacement answer",
	}); err != nil {
		t.Fatalf("complete reclaimed turn: %v", err)
	}

	// 7. session/user 不匹配（无归属关系）必须返回 ErrSessionNotFound，不能把租约挂到别人会话上。
	_, err = turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          otherUser,
		SessionID:       sessionID,
		ClientMessageID: "00000000-0000-4000-8000-000000000094",
		Content:         "foreign question",
		LeaseDuration:   time.Minute,
	})
	if !errors.Is(err, service.ErrSessionNotFound) {
		t.Fatalf("mismatched owner acquire error = %v, want ErrSessionNotFound", err)
	}
}

// TestPostgresTurnLeaseRepositoryAcquireReopensFailedLeaseForRetry 验证"上一次失败后重试"这条
// 结果恢复路径：同一个 client_message_id 在租约变成 failed 之后再次获取，必须原地把这一行改回
// active（不是插入新行），因为 (session_id, client_message_id) 是全局唯一约束，同一条消息永远
// 只能有一条租约记录。
func TestPostgresTurnLeaseRepositoryAcquireReopensFailedLeaseForRetry(t *testing.T) {
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
		userID    = "usr_turn_lease_retry_test"
		sessionID = "session_00000000000000000000000000000095"
	)
	ctx := context.Background()
	cleanupTurnLeaseRepositoryTest(t, pool, userID)
	defer cleanupTurnLeaseRepositoryTest(t, pool, userID)

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2)`, sessionID, userID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	turnLeaseService := service.NewTurnLeaseService(store.NewPostgresTurnLeaseRepository(pool))
	messageID := "00000000-0000-4000-8000-000000000095"

	first, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: messageID,
		Content:         "retry question",
		LeaseDuration:   time.Minute,
	})
	if err != nil || !first.Acquired {
		t.Fatalf("first acquire: result=%+v err=%v", first, err)
	}

	// 上一次处理确实失败了（例如 LLM 报错、超时或 assistant 落库失败）。
	if err := turnLeaseService.Release(ctx, service.ReleaseTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: messageID,
		AttemptNo:       first.Lease.AttemptNo,
		Status:          service.TurnLeaseFailed,
	}); err != nil {
		t.Fatalf("release as failed: %v", err)
	}

	// 客户端用同一个 client_message_id 重试：必须重新拿到 active 租约，且是原地复用同一行
	// （而不是新建一行），否则会撞上 (session_id, client_message_id) 的唯一约束。
	retry, err := turnLeaseService.Acquire(ctx, service.AcquireTurnLeaseRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: messageID,
		Content:         "retry question",
		LeaseDuration:   time.Minute,
	})
	if err != nil {
		t.Fatalf("retry acquire after failure: %v", err)
	}
	if !retry.Acquired || retry.Lease.ID != first.Lease.ID || retry.Lease.Status != service.TurnLeaseActive {
		t.Fatalf("retry acquire result = %+v, want acquired + same row id %d + active", retry, first.Lease.ID)
	}
	if retry.Lease.AttemptNo != first.Lease.AttemptNo+1 {
		t.Fatalf("retry attempt_no = %d, want %d", retry.Lease.AttemptNo, first.Lease.AttemptNo+1)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_turn_lease WHERE session_id = $1 AND client_message_id = $2`,
		sessionID, messageID).Scan(&rowCount); err != nil {
		t.Fatalf("count lease rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("lease row count = %d, want exactly 1 (reopened in place, not duplicated)", rowCount)
	}

	_, err = turnLeaseService.Complete(ctx, service.CompleteTurnRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: messageID,
		AttemptNo:       first.Lease.AttemptNo,
		UserMessageID:   first.UserMessage.MessageID,
		Content:         "stale answer",
	})
	if !errors.Is(err, service.ErrTurnLeaseLost) {
		t.Fatalf("stale Complete() error = %v, want ErrTurnLeaseLost", err)
	}
	var staleReplyCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_memory_episodic
		WHERE session_id = $1 AND parent_message_id = $2 AND role = 'assistant'`, sessionID, first.UserMessage.MessageID).Scan(&staleReplyCount); err != nil {
		t.Fatalf("count stale replies: %v", err)
	}
	if staleReplyCount != 0 {
		t.Fatalf("stale reply count = %d, want 0", staleReplyCount)
	}

	if _, err := turnLeaseService.Complete(ctx, service.CompleteTurnRequest{
		UserID:          userID,
		SessionID:       sessionID,
		ClientMessageID: messageID,
		AttemptNo:       retry.Lease.AttemptNo,
		UserMessageID:   retry.UserMessage.MessageID,
		Content:         "retry answer",
	}); err != nil {
		t.Fatalf("complete after retry: %v", err)
	}
}

// TestPostgresTurnLeaseRepositoryAcquireIsSerializedAcrossInstances 用两个独立的连接池
// （模拟两个进程/两个 repository 实例）并发抢同一个 Session 的 turn，验证互斥由数据库短事务
// 保证，而不是靠 Go 进程内的内存锁。
func TestPostgresTurnLeaseRepositoryAcquireIsSerializedAcrossInstances(t *testing.T) {
	if os.Getenv("INTERVIEW_AGENT_INTEGRATION_TEST") != "1" {
		t.Skip("set INTERVIEW_AGENT_INTEGRATION_TEST=1 to run PostgreSQL integration tests")
	}

	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	poolA, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres pool A: %v", err)
	}
	defer poolA.Close()
	if err := store.RunMigrations(cfg.Postgres, os.DirFS("../../migrations"), "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	poolB, err := store.NewPostgres(cfg.Postgres)
	if err != nil {
		t.Fatalf("connect postgres pool B: %v", err)
	}
	defer poolB.Close()

	const (
		userID    = "usr_turn_lease_cross_instance_test"
		sessionID = "session_00000000000000000000000000000095"
	)
	ctx := context.Background()
	cleanupTurnLeaseRepositoryTest(t, poolA, userID)
	defer cleanupTurnLeaseRepositoryTest(t, poolA, userID)

	if _, err := poolA.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := poolA.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2)`, sessionID, userID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	serviceA := service.NewTurnLeaseService(store.NewPostgresTurnLeaseRepository(poolA))
	serviceB := service.NewTurnLeaseService(store.NewPostgresTurnLeaseRepository(poolB))

	const attempts = 8
	type outcome struct {
		acquired bool
		err      error
	}
	results := make(chan outcome, attempts)
	for index := 0; index < attempts; index++ {
		turnService, clientMessageID := serviceA, fmt.Sprintf("00000000-0000-4000-8000-1%011d", index)
		if index%2 == 1 {
			turnService = serviceB
		}
		go func(turnService *service.TurnLeaseService, clientMessageID string) {
			result, acquireErr := turnService.Acquire(ctx, service.AcquireTurnLeaseRequest{
				UserID:          userID,
				SessionID:       sessionID,
				ClientMessageID: clientMessageID,
				Content:         "parallel question " + clientMessageID,
				LeaseDuration:   time.Minute,
			})
			results <- outcome{acquired: result.Acquired, err: acquireErr}
		}(turnService, clientMessageID)
	}

	successes := 0
	conflicts := 0
	for index := 0; index < attempts; index++ {
		result := <-results
		switch {
		case result.err == nil && result.acquired:
			successes++
		case errors.Is(result.err, service.ErrTurnLeaseConflict):
			conflicts++
		default:
			t.Fatalf("unexpected acquire outcome: %+v", result)
		}
	}
	if successes != 1 || conflicts != attempts-1 {
		t.Fatalf("successes=%d conflicts=%d, want exactly 1 success and %d conflicts", successes, conflicts, attempts-1)
	}

	var activeCount int
	if err := poolA.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_turn_lease WHERE session_id = $1 AND status = 'active'`,
		sessionID).Scan(&activeCount); err != nil {
		t.Fatalf("count active leases: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("active lease count = %d, want 1", activeCount)
	}
}

func cleanupTurnLeaseRepositoryTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
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
