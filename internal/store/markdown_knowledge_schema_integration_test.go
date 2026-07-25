package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"healthAgent/internal/config"
	"healthAgent/internal/store"
)

func TestMarkdownKnowledgeSchemaPreservesOwnershipAndLineage(t *testing.T) {
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

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback transaction: %v", err)
		}
	}()

	const (
		userID           = "usr_markdown_schema_test"
		otherUserID      = "usr_markdown_schema_other"
		documentID       = "00000000-0000-4000-8000-000000000601"
		duplicateDocID   = "00000000-0000-4000-8000-000000000602"
		versionID        = "00000000-0000-4000-8000-000000000611"
		duplicateVerID   = "00000000-0000-4000-8000-000000000612"
		chunkID          = "00000000-0000-4000-8000-000000000621"
		knowledgeID      = "00000000-0000-4000-8000-000000000631"
		otherKnowledgeID = "00000000-0000-4000-8000-000000000632"
	)

	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_user (user_id, user_type, status)
		VALUES ($1, 0, 0), ($2, 0, 0)`, userID, otherUserID); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO documents (document_id, user_id, title, created_by, updated_by)
		VALUES ($1, $3, 'Concurrency notes', $3, $3),
		       ($2, $3, 'A separately saved copy', $3, $3)`, documentID, duplicateDocID, userID); err != nil {
		t.Fatalf("insert documents: %v", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO document_versions (
			version_id, document_id, user_id, version_no, original_filename,
			mime_type, size_bytes, sha256, raw_text, parser_version, created_by, updated_by
		) VALUES (
			$1, $2, $3, 1, 'notes.md', 'text/markdown', 14,
			decode(repeat('ab', 32), 'hex'), E'# Heading\nbody', 'markdown-v0', $3, $3
		)`, versionID, documentID, userID); err != nil {
		t.Fatalf("insert first document version: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE documents SET current_version_id = $1, updated_by = $3
		WHERE document_id = $2 AND user_id = $3`, versionID, documentID, userID); err != nil {
		t.Fatalf("set current version: %v", err)
	}

	// user_id + sha256 is intentionally a lookup index, not a uniqueness rule:
	// duplicate content can be detected without silently merging two user records.
	if _, err := tx.Exec(ctx, `
		INSERT INTO document_versions (
			version_id, document_id, user_id, version_no, original_filename,
			mime_type, size_bytes, sha256, raw_text, parser_version, created_by, updated_by
		) VALUES (
			$1, $2, $3, 1, 'copy.md', 'text/markdown', 14,
			decode(repeat('ab', 32), 'hex'), E'# Heading\nbody', 'markdown-v0', $3, $3
		)`, duplicateVerID, duplicateDocID, userID); err != nil {
		t.Fatalf("insert separately saved duplicate content: %v", err)
	}

	assertPostgresState(t, ctx, tx, "23503", func() error {
		_, err := tx.Exec(ctx, `
			INSERT INTO document_versions (
				version_id, document_id, user_id, version_no, original_filename,
				mime_type, size_bytes, sha256, raw_text, parser_version, created_by, updated_by
			) VALUES (
				'00000000-0000-4000-8000-000000000613', $1, $2, 2, 'foreign.md',
				'text/markdown', 4, decode(repeat('cd', 32), 'hex'), 'body', 'markdown-v0', $2, $2
			)`, documentID, otherUserID)
		return err
	})

	assertPostgresState(t, ctx, tx, "55000", func() error {
		_, err := tx.Exec(ctx, `UPDATE document_versions SET raw_text = 'overwritten' WHERE version_id = $1`, versionID)
		return err
	})

	if _, err := tx.Exec(ctx, `
		INSERT INTO source_chunks (
			source_chunk_id, document_version_id, document_id, user_id, ordinal,
			heading_path, content, start_offset, end_offset, content_hash, created_by, updated_by
		) VALUES (
			$1, $2, $3, $4, 1, ARRAY['Heading'], 'body', 10, 14,
			decode(repeat('ef', 32), 'hex'), $4, $4
		)`, chunkID, versionID, documentID, userID); err != nil {
		t.Fatalf("insert source chunk: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE source_chunks
		SET retrieval_enabled = TRUE, trust_level = 'user_confirmed', updated_by = $2
		WHERE source_chunk_id = $1`, chunkID, userID); err != nil {
		t.Fatalf("update mutable source chunk policy: %v", err)
	}
	assertPostgresState(t, ctx, tx, "55000", func() error {
		_, err := tx.Exec(ctx, `UPDATE source_chunks SET content = 'changed' WHERE source_chunk_id = $1`, chunkID)
		return err
	})

	if _, err := tx.Exec(ctx, `
		INSERT INTO document_usages (
			usage_id, document_version_id, document_id, user_id, purpose, enabled,
			confirmed_by, confirmed_at, created_by, updated_by
		) VALUES (
			'00000000-0000-4000-8000-000000000641', $1, $2, $3, 'learn', TRUE,
			$3, now(), $3, $3
		)`, versionID, documentID, userID); err != nil {
		t.Fatalf("insert learning usage: %v", err)
	}
	assertPostgresState(t, ctx, tx, "23514", func() error {
		_, err := tx.Exec(ctx, `
			INSERT INTO document_usages (
				usage_id, document_version_id, document_id, user_id, purpose, enabled,
				confirmed_by, confirmed_at, created_by, updated_by
			) VALUES (
				'00000000-0000-4000-8000-000000000642', $1, $2, $3, 'archive_only', TRUE,
				$3, now(), $3, $3
			)`, versionID, documentID, userID)
		return err
	})

	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_points (
			knowledge_point_id, user_id, title, created_by, updated_by
		) VALUES ($1, $3, 'Bounded concurrency', $3, $3),
		         ($2, $4, 'Other user knowledge', $4, $4)`, knowledgeID, otherKnowledgeID, userID, otherUserID); err != nil {
		t.Fatalf("insert knowledge point: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_point_sources (
			knowledge_point_id, source_chunk_id, user_id, relation_type, created_by, updated_by
		) VALUES ($1, $2, $3, 'primary', $3, $3)`, knowledgeID, chunkID, userID); err != nil {
		t.Fatalf("link knowledge point source: %v", err)
	}
	assertPostgresState(t, ctx, tx, "23503", func() error {
		_, err := tx.Exec(ctx, `
			INSERT INTO knowledge_point_sources (
				knowledge_point_id, source_chunk_id, user_id, relation_type, created_by, updated_by
			) VALUES ($1, $2, $3, 'reference', $3, $3)`, otherKnowledgeID, chunkID, otherUserID)
		return err
	})

	candidateTypes := []string{
		"knowledge_point",
		"plan_task",
		"jd_requirement",
		"personal_fact",
		"reference_only",
	}
	for index, candidateType := range candidateTypes {
		candidateID := candidateUUID(index + 1)
		if _, err := tx.Exec(ctx, `
			INSERT INTO content_candidates (
				candidate_id, user_id, candidate_type, candidate_payload, created_by, updated_by
			) VALUES ($1, $2, $3::text, jsonb_build_object('title', $3::text), $2, $2)`, candidateID, userID, candidateType); err != nil {
			t.Fatalf("insert %s candidate: %v", candidateType, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO content_candidate_sources (
				candidate_id, source_chunk_id, user_id, source_order, evidence_quote, created_by, updated_by
			) VALUES ($1, $2, $3, 1, 'body', $3, $3)`, candidateID, chunkID, userID); err != nil {
			t.Fatalf("link %s candidate source: %v", candidateType, err)
		}
	}
}

func assertPostgresState(t *testing.T, ctx context.Context, tx pgx.Tx, wantCode string, operation func() error) {
	t.Helper()

	if _, err := tx.Exec(ctx, "SAVEPOINT expected_schema_failure"); err != nil {
		t.Fatalf("create savepoint: %v", err)
	}
	operationErr := operation()

	var pgError *pgconn.PgError
	if !errors.As(operationErr, &pgError) || pgError.Code != wantCode {
		t.Errorf("operation error = %v, want PostgreSQL state %s", operationErr, wantCode)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT expected_schema_failure"); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT expected_schema_failure"); err != nil {
		t.Fatalf("release savepoint: %v", err)
	}
}

func candidateUUID(sequence int) string {
	return "00000000-0000-4000-8000-00000000065" + string(rune('0'+sequence))
}
