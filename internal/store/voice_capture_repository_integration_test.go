package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/service"
	"KnowledgeMirror/internal/store"
)

func TestPostgresVoiceCaptureRepositoryLifecycle(t *testing.T) {
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
		userID       = "usr_voice_capture_test"
		otherUserID  = "usr_voice_capture_other"
		sessionID    = "session_00000000000000000000000000000120"
		otherSession = "session_00000000000000000000000000000121"
	)
	ctx := context.Background()
	cleanupVoiceCaptureTest(t, pool, userID, otherUserID)
	defer cleanupVoiceCaptureTest(t, pool, userID, otherUserID)

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

	repo := store.NewPostgresVoiceCaptureRepository(pool)
	audio := []byte("fake-audio-bytes")
	hash := sha256Bytes(audio)

	newClaimParams := func(captureID string, staleBefore time.Time) service.ClaimVoiceCaptureParams {
		return service.ClaimVoiceCaptureParams{
			CaptureID:   captureID,
			UserID:      userID,
			SessionID:   sessionID,
			MIMEType:    "audio/webm",
			SizeBytes:   int64(len(audio)),
			SHA256:      hash,
			AudioData:   audio,
			STTProvider: "openai_whisper",
			StaleBefore: staleBefore,
		}
	}

	// 1. 会话不属于当前用户时必须直接拒绝，不能把录音写进别人的会话。
	foreign := newClaimParams(mustNewCaptureID(t), time.Now().Add(-2*time.Minute))
	foreign.SessionID = otherSession
	if _, _, err := repo.Claim(ctx, foreign); !errors.Is(err, service.ErrSessionNotFound) {
		t.Fatalf("Claim() cross-user error = %v, want ErrSessionNotFound", err)
	}

	// 2. 首次抢占。
	captureID := mustNewCaptureID(t)
	claimed, won, err := repo.Claim(ctx, newClaimParams(captureID, time.Now().Add(-2*time.Minute)))
	if err != nil || !won {
		t.Fatalf("Claim() first = (won=%t, %v), want (true, nil)", won, err)
	}
	if claimed.Status != service.VoiceCaptureStatusTranscribing || claimed.CaptureID != captureID {
		t.Fatalf("Claim() = %+v, want transcribing 状态的新记录", claimed)
	}

	// 3. 相同字节重复提交命中去重，won=false（调用方据此跳过 STT，不重复计费）。
	duplicate, won, err := repo.Claim(ctx, newClaimParams(mustNewCaptureID(t), time.Now().Add(-2*time.Minute)))
	if err != nil {
		t.Fatalf("Claim() duplicate: %v", err)
	}
	if won || duplicate.CaptureID != captureID {
		t.Fatalf("Claim() duplicate = (won=%t, capture=%s), want (false, %s)", won, duplicate.CaptureID, captureID)
	}

	// 4. 写入终态。
	confidence := 0.42
	completed, err := repo.Complete(ctx, service.CompleteVoiceCaptureParams{
		CaptureID:          captureID,
		UserID:             userID,
		Status:             service.VoiceCaptureStatusTranscribed,
		STTProvider:        "openai_whisper",
		STTModel:           "whisper-1",
		STTRequestID:       "req-1",
		RawTranscript:      "这段链路是靠密等挡住重复请求的",
		Confidence:         &confidence,
		AmbiguousTerms:     []service.AmbiguousTerm{{Term: "幂等", Heard: "密等"}},
		NeedsConfirmation:  true,
		ConfirmationReason: service.VoiceConfirmReasonLowConfidence,
	})
	if err != nil {
		t.Fatalf("Complete(): %v", err)
	}
	if completed.Status != service.VoiceCaptureStatusTranscribed ||
		completed.Confidence == nil || *completed.Confidence != confidence ||
		len(completed.AmbiguousTerms) != 1 || completed.AmbiguousTerms[0].Term != "幂等" ||
		completed.ConfirmationReason != service.VoiceConfirmReasonLowConfidence {
		t.Fatalf("Complete() = %+v, want 完整落库的终态", completed)
	}

	// 5. 终态后原始转写不可变：这是“不静默覆盖历史”的最后一道防线。
	if _, err := pool.Exec(ctx, `
		UPDATE voice_captures SET raw_transcript = '被偷偷改掉的转写' WHERE capture_id = $1::uuid`,
		captureID); err == nil {
		t.Fatal("UPDATE raw_transcript error = nil, want 触发器拒绝")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("UPDATE raw_transcript error = %v, want immutable 触发器异常", err)
	}

	// 6. 记录不允许删除。
	if _, err := pool.Exec(ctx, `DELETE FROM voice_captures WHERE capture_id = $1::uuid`, captureID); err == nil {
		t.Fatal("DELETE error = nil, want 触发器拒绝")
	}

	// 7. 绑定消息：先落一条真实用户消息，再把转写挂上去。
	messageID := mustNewCaptureID(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_episodic (message_id, session_id, user_id, agent_id, seq, role, status, content)
		VALUES ($1::uuid, $2, $3, 'interview-agent', 1, 'user', 'completed', '这段链路是靠幂等挡住重复请求的')`,
		messageID, sessionID, userID); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := repo.BindMessage(ctx, userID, sessionID, captureID, messageID); err != nil {
		t.Fatalf("BindMessage(): %v", err)
	}
	// 重复绑定同一条消息保持幂等（前端重试不该报错）。
	if err := repo.BindMessage(ctx, userID, sessionID, captureID, messageID); err != nil {
		t.Fatalf("BindMessage() repeat: %v", err)
	}
	// 改绑到另一条消息必须失败：一段转写只能对应一次发送。
	otherMessageID := mustNewCaptureID(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_episodic (message_id, session_id, user_id, agent_id, seq, role, status, content)
		VALUES ($1::uuid, $2, $3, 'interview-agent', 2, 'user', 'completed', '另一条消息')`,
		otherMessageID, sessionID, userID); err != nil {
		t.Fatalf("insert other message: %v", err)
	}
	if err := repo.BindMessage(ctx, userID, sessionID, captureID, otherMessageID); err == nil {
		t.Fatal("BindMessage() rebind error = nil, want 拒绝改绑")
	}

	// 8. 越权读取必须查不到，而不是返回别人的转写。
	if _, err := repo.Get(ctx, otherUserID, sessionID, captureID); !errors.Is(err, service.ErrVoiceCaptureNotFound) {
		t.Fatalf("Get() cross-user error = %v, want ErrVoiceCaptureNotFound", err)
	}
	reloaded, err := repo.Get(ctx, userID, sessionID, captureID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if reloaded.MessageID != messageID || reloaded.RawTranscript != completed.RawTranscript {
		t.Fatalf("Get() = %+v, want 绑定后的完整记录", reloaded)
	}
}

// 卡在 transcribing 的记录（上次调用崩了）必须能被后续请求接管重试，
// 否则用户永远拿不回那段话，只能换一句重录。
func TestPostgresVoiceCaptureRepositoryTakesOverStaleTranscribing(t *testing.T) {
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

	const (
		userID    = "usr_voice_capture_stale"
		sessionID = "session_00000000000000000000000000000122"
	)
	ctx := context.Background()
	cleanupVoiceCaptureTest(t, pool, userID)
	defer cleanupVoiceCaptureTest(t, pool, userID)

	if _, err := pool.Exec(ctx, `INSERT INTO agent_user (user_id, user_type, status) VALUES ($1, 0, 0)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_memory_session (session_id, user_id) VALUES ($1, $2)`,
		sessionID, userID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	repo := store.NewPostgresVoiceCaptureRepository(pool)
	audio := []byte("stale-audio-bytes")
	params := service.ClaimVoiceCaptureParams{
		CaptureID: mustNewCaptureID(t), UserID: userID, SessionID: sessionID,
		MIMEType: "audio/webm", SizeBytes: int64(len(audio)), SHA256: sha256Bytes(audio),
		AudioData: audio, STTProvider: "openai_whisper", StaleBefore: time.Now().Add(-2 * time.Minute),
	}
	first, won, err := repo.Claim(ctx, params)
	if err != nil || !won {
		t.Fatalf("Claim() first = (won=%t, %v)", won, err)
	}

	// 未过期时不接管：并发的同一段录音应该等前一次结果，而不是重复调用 STT。
	if _, won, err := repo.Claim(ctx, params); err != nil || won {
		t.Fatalf("Claim() fresh duplicate = (won=%t, %v), want (false, nil)", won, err)
	}

	// StaleBefore 推到未来，模拟这条记录已经卡了很久。
	params.CaptureID = mustNewCaptureID(t)
	params.StaleBefore = time.Now().Add(time.Minute)
	takenOver, won, err := repo.Claim(ctx, params)
	if err != nil || !won {
		t.Fatalf("Claim() stale = (won=%t, %v), want (true, nil)", won, err)
	}
	if takenOver.CaptureID != first.CaptureID {
		t.Fatalf("接管应复用同一条记录: %s vs %s", takenOver.CaptureID, first.CaptureID)
	}
	if takenOver.Status != service.VoiceCaptureStatusTranscribing {
		t.Fatalf("Status = %q, want transcribing", takenOver.Status)
	}
}

func mustNewCaptureID(t *testing.T) string {
	t.Helper()
	id, err := service.NewVoiceCaptureID()
	if err != nil {
		t.Fatalf("new capture id: %v", err)
	}
	return id
}

func sha256Bytes(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}

// cleanupVoiceCaptureTest 用 `SET LOCAL session_replication_role = replica` 临时关闭触发器：
// voice_captures 的守卫触发器禁止 DELETE，测试专用清理只能这样绕。
// 该设置随事务结束自动还原，不影响连接池里其它连接。
func cleanupVoiceCaptureTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("cleanup begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("cleanup disable triggers: %v", err)
	}
	statements := []string{
		`DELETE FROM voice_captures WHERE user_id = $1`,
		`DELETE FROM feynman_practice_states WHERE user_id = $1`,
		`DELETE FROM agent_turn_lease WHERE user_id = $1`,
		`DELETE FROM agent_memory_episodic WHERE user_id = $1`,
		`DELETE FROM agent_memory_session WHERE user_id = $1`,
		`DELETE FROM agent_user WHERE user_id = $1`,
	}
	for _, userID := range userIDs {
		for _, sql := range statements {
			if _, err := tx.Exec(ctx, sql, userID); err != nil {
				t.Fatalf("cleanup exec %q: %v", sql, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("cleanup commit tx: %v", err)
	}
}
