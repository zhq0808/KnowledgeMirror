package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/config"
	"KnowledgeMirror/internal/store"
)

func TestPostgresSessionRepositoryListsOwnedSessionsDeterministically(t *testing.T) {
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
		userID      = "usr_session_repository_test"
		otherUserID = "usr_session_repository_other"
		oldSession  = "session_00000000000000000000000000000080"
		archived    = "session_00000000000000000000000000000081"
		newestOnTie = "session_00000000000000000000000000000082"
		deleted     = "session_00000000000000000000000000000083"
		foreign     = "session_00000000000000000000000000000084"
	)
	ctx := context.Background()
	cleanupSessionRepositoryTest(t, pool, userID, otherUserID)
	defer cleanupSessionRepositoryTest(t, pool, userID, otherUserID)

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0), ($2, 0, 0)`, userID, otherUserID); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	tieTime := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_memory_session
			(session_id, user_id, title, status, message_count, last_message_at, deleted_at, created_at)
		VALUES
			($1, $5, 'old',      'active',   1, $7, NULL, $6),
			($2, $5, 'archived', 'archived', 2, $8, NULL, $8),
			($3, $5, 'newest',   'active',   3, $8, NULL, $8),
			($4, $5, 'deleted',  'active',   4, $9, $9,   $9),
			($10, $11, 'foreign','active',   5, $9, NULL, $9)`,
		oldSession,
		archived,
		newestOnTie,
		deleted,
		userID,
		tieTime.Add(-3*time.Hour),
		tieTime.Add(-2*time.Hour),
		tieTime,
		tieTime.Add(time.Hour),
		foreign,
		otherUserID,
	); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	repository := store.NewPostgresSessionRepository(pool)
	items, err := repository.ListSessions(ctx, userID, 2)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(items) != 2 || items[0].SessionID != newestOnTie || items[1].SessionID != archived {
		t.Fatalf("ListSessions() = %+v, want deterministic [%s, %s]", items, newestOnTie, archived)
	}
	if items[1].Status != "archived" || items[1].MessageCount != 2 {
		t.Fatalf("archived item = %+v, want status archived and message_count 2", items[1])
	}

	assertSessionOwnership(t, repository, ctx, userID, newestOnTie, true, true)
	assertSessionOwnership(t, repository, ctx, userID, archived, true, false)
	assertSessionOwnership(t, repository, ctx, userID, deleted, false, false)
	assertSessionOwnership(t, repository, ctx, userID, foreign, false, false)
}

type sessionOwnershipRepository interface {
	OwnsSession(context.Context, string, string) (bool, error)
	OwnsActiveSession(context.Context, string, string) (bool, error)
}

func assertSessionOwnership(t *testing.T, repository sessionOwnershipRepository, ctx context.Context, userID, sessionID string, wantOwned, wantActive bool) {
	t.Helper()
	owned, err := repository.OwnsSession(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("OwnsSession(%s) error = %v", sessionID, err)
	}
	active, err := repository.OwnsActiveSession(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("OwnsActiveSession(%s) error = %v", sessionID, err)
	}
	if owned != wantOwned || active != wantActive {
		t.Fatalf("session %s ownership = (%t, %t), want (%t, %t)", sessionID, owned, active, wantOwned, wantActive)
	}
}

func cleanupSessionRepositoryTest(t *testing.T, pool *pgxpool.Pool, userIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, userID := range userIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM agent_memory_session WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup sessions: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM agent_user WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("cleanup user: %v", err)
		}
	}
}
