package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"healthAgent/internal/config"
	"healthAgent/internal/service"
	"healthAgent/internal/store"
)

func TestPostgresFeynmanPracticeRepositoryLifecycle(t *testing.T) {
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
		userID       = "usr_feynman_practice_test"
		otherUserID  = "usr_feynman_practice_other"
		sessionID    = "session_00000000000000000000000000000110"
		otherSession = "session_00000000000000000000000000000111"
	)
	ctx := context.Background()
	cleanupFeynmanPracticeTest(t, pool, userID, otherUserID)
	defer cleanupFeynmanPracticeTest(t, pool, userID, otherUserID)

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0), ($2, 0, 0)`, userID, otherUserID); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session (session_id, user_id)
		VALUES ($1, $2), ($3, $4)`, sessionID, userID, otherSession, otherUserID); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	repo := store.NewPostgresFeynmanPracticeRepository(pool)

	// 1. 没有记录时按“未开始练习”处理，而不是报错。
	if _, found, err := repo.Load(ctx, userID, sessionID); err != nil || found {
		t.Fatalf("Load() on empty = (%v, %v), want (false, nil)", found, err)
	}

	// 2. 首次写入。
	first := service.FeynmanPracticeState{
		SessionID:          sessionID,
		UserID:             userID,
		State:              service.FeynmanStateAwaitingAnswer,
		ActiveQuestionText: "为什么当时选 Kafka",
		QuestionOrigin:     service.FeynmanQuestionOriginUserTopic,
		RoundNo:            1,
	}
	if err := repo.Save(ctx, first); err != nil {
		t.Fatalf("Save() first: %v", err)
	}
	loaded, found, err := repo.Load(ctx, userID, sessionID)
	if err != nil || !found {
		t.Fatalf("Load() after save = (%v, %v)", found, err)
	}
	if loaded.State != first.State || loaded.ActiveQuestionText != first.ActiveQuestionText || loaded.RoundNo != 1 {
		t.Fatalf("Load() = %+v, want %+v", loaded, first)
	}
	if loaded.LastAnsweredMessageID != "" || loaded.LastFeedback != "" {
		t.Fatalf("Load() = %+v, want empty answer/feedback", loaded)
	}

	// 3. 同一会话再写一次是覆盖而不是新增：这张表是当前投影，不是历史账本。
	second := first
	second.State = service.FeynmanStateAwaitingFollowUp
	second.RoundNo = 2
	if err := repo.Save(ctx, second); err != nil {
		t.Fatalf("Save() second: %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM feynman_practice_states WHERE user_id = $1`, userID).Scan(&rowCount); err != nil {
		t.Fatalf("count states: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("state row count = %d, want 1", rowCount)
	}
	loaded, _, err = repo.Load(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("Load() after upsert: %v", err)
	}
	if loaded.State != service.FeynmanStateAwaitingFollowUp || loaded.RoundNo != 2 {
		t.Fatalf("Load() = %+v, want follow-up round 2", loaded)
	}

	// 4. 跨用户读不到，也写不动别人的会话。
	if _, found, err := repo.Load(ctx, otherUserID, sessionID); err != nil || found {
		t.Fatalf("cross-user Load() = (%v, %v), want (false, nil)", found, err)
	}
	hijack := first
	hijack.UserID = otherUserID
	hijack.ActiveQuestionText = "被篡改的题目"
	if err := repo.Save(ctx, hijack); err == nil {
		t.Fatal("cross-user Save() error = nil, want rejection")
	}
	loaded, _, err = repo.Load(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("Load() after hijack attempt: %v", err)
	}
	if loaded.ActiveQuestionText != first.ActiveQuestionText {
		t.Fatalf("cross-user Save() 改动了他人状态: %+v", loaded)
	}

	// 5. 数据库兜底：等待回答却没有题目属于非法状态，必须被 CHECK 拦下。
	invalid := first
	invalid.ActiveQuestionText = "   "
	if err := repo.Save(ctx, invalid); err == nil {
		t.Fatal("Save() with empty question error = nil, want CHECK violation")
	} else if !strings.Contains(err.Error(), "ck_feynman_practice_states_question") {
		t.Fatalf("Save() error = %v, want question CHECK violation", err)
	}

	// 6. 非法状态取值同样由数据库兜底，服务层枚举不是唯一防线。
	unknown := first
	unknown.State = "not_a_state"
	if err := repo.Save(ctx, unknown); err == nil {
		t.Fatal("Save() with unknown state error = nil, want CHECK violation")
	}
}

func cleanupFeynmanPracticeTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, userID := range userIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM feynman_practice_states WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup practice states: %v", err)
		}
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
