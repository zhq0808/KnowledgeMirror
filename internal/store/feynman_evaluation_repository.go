package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"healthAgent/internal/service"
)

const feynmanEvaluationSelectSQL = `
	SELECT e.evaluation_id::text, e.attempt_id::text, e.confirmation_id::text,
	       e.rubric_id::text, e.knowledge_point_id::text, e.user_id, e.status,
	       e.prompt_version, e.model_name, COALESCE(e.retrieval_request_id::text, ''),
	       e.confirmed_transcript_hash, e.result_payload, e.source_snapshots,
	       COALESCE(e.error_message, ''), e.created_at, e.updated_at,
	       c.confirmed_transcript,
	       COALESCE(d.decision_id::text, ''), COALESCE(d.decision, ''), d.final_payload,
	       COALESCE(d.decision_note, ''), COALESCE(d.decided_by, ''), d.decided_at
	FROM feynman_evaluations e
	JOIN feynman_transcript_confirmations c
	  ON c.confirmation_id = e.confirmation_id AND c.attempt_id = e.attempt_id AND c.user_id = e.user_id
	LEFT JOIN feynman_evaluation_decisions d
	  ON d.evaluation_id = e.evaluation_id AND d.user_id = e.user_id`

func scanFeynmanEvaluation(row rowScanner) (service.FeynmanEvaluation, error) {
	var evaluation service.FeynmanEvaluation
	var resultJSON, sourcesJSON, finalJSON []byte
	var decisionID, decision, decisionNote, decidedBy string
	var decidedAt *time.Time
	if err := row.Scan(
		&evaluation.EvaluationID, &evaluation.AttemptID, &evaluation.ConfirmationID,
		&evaluation.RubricID, &evaluation.KnowledgePointID, &evaluation.UserID, &evaluation.Status,
		&evaluation.PromptVersion, &evaluation.ModelName, &evaluation.RetrievalRequestID,
		&evaluation.ConfirmedTranscriptHash, &resultJSON, &sourcesJSON,
		&evaluation.ErrorMessage, &evaluation.CreatedAt, &evaluation.UpdatedAt,
		&evaluation.ConfirmedTranscript,
		&decisionID, &decision, &finalJSON, &decisionNote, &decidedBy, &decidedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation, service.ErrFeynmanEvaluationNotFound
		}
		return evaluation, fmt.Errorf("扫描费曼评估失败: %w", err)
	}
	if len(resultJSON) > 0 {
		var payload service.FeynmanEvaluationPayload
		if err := json.Unmarshal(resultJSON, &payload); err != nil {
			return evaluation, fmt.Errorf("解析费曼评估结果失败: %w", err)
		}
		evaluation.Payload = &payload
	}
	if err := json.Unmarshal(sourcesJSON, &evaluation.Sources); err != nil {
		return evaluation, fmt.Errorf("解析费曼评估来源失败: %w", err)
	}
	if decisionID != "" {
		decisionRecord := &service.FeynmanEvaluationDecision{
			DecisionID: decisionID, EvaluationID: evaluation.EvaluationID, UserID: evaluation.UserID,
			Decision: decision, DecisionNote: decisionNote, DecidedBy: decidedBy,
		}
		if decidedAt != nil {
			decisionRecord.DecidedAt = *decidedAt
		}
		if len(finalJSON) > 0 {
			var payload service.FeynmanEvaluationPayload
			if err := json.Unmarshal(finalJSON, &payload); err != nil {
				return evaluation, fmt.Errorf("解析费曼评估最终内容失败: %w", err)
			}
			decisionRecord.FinalPayload = &payload
		}
		evaluation.Decision = decisionRecord
	}
	return evaluation, nil
}

func (r *PostgresFeynmanRepository) GetKnowledgePointTitle(ctx context.Context, userID, knowledgePointID string) (string, error) {
	var title string
	err := r.pool.QueryRow(ctx, `
		SELECT title FROM knowledge_points
		WHERE knowledge_point_id = $1 AND user_id = $2 AND deleted_at IS NULL`, knowledgePointID, userID).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", service.ErrFeynmanKnowledgePointNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询知识点标题失败: %w", err)
	}
	return title, nil
}

func (r *PostgresFeynmanRepository) ClaimEvaluation(ctx context.Context, params service.ClaimFeynmanEvaluationParams) (service.FeynmanEvaluation, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.FeynmanEvaluation{}, false, fmt.Errorf("开启费曼评估事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var confirmationID, rubricID string
	err = tx.QueryRow(ctx, `
		SELECT a.confirmation_id::text, kp.current_rubric_version_id::text
		FROM feynman_attempts a
		JOIN knowledge_points kp ON kp.knowledge_point_id = a.knowledge_point_id AND kp.user_id = a.user_id
		WHERE a.attempt_id = $1 AND a.user_id = $2 AND a.knowledge_point_id = $3
		FOR UPDATE OF a`, params.AttemptID, params.UserID, params.KnowledgePointID).Scan(&confirmationID, &rubricID)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.FeynmanEvaluation{}, false, service.ErrFeynmanAttemptNotFound
	}
	if err != nil {
		return service.FeynmanEvaluation{}, false, fmt.Errorf("锁定费曼练习失败: %w", err)
	}
	if confirmationID != params.ConfirmationID || rubricID != params.RubricID {
		return service.FeynmanEvaluation{}, false, fmt.Errorf("费曼评估输入版本已变化")
	}

	var existingID, status, promptVersion, modelName string
	err = tx.QueryRow(ctx, `
		SELECT evaluation_id::text, status, prompt_version, model_name
		FROM feynman_evaluations WHERE attempt_id = $1 FOR UPDATE`, params.AttemptID).
		Scan(&existingID, &status, &promptVersion, &modelName)
	claimed := false
	if err == nil {
		if status == service.FeynmanEvaluationStatusFailed {
			if promptVersion != params.PromptVersion || modelName != params.ModelName {
				return service.FeynmanEvaluation{}, false, fmt.Errorf("失败评估必须使用原 Prompt 和模型重试")
			}
			if _, err := tx.Exec(ctx, `
				UPDATE feynman_evaluations
				SET status = 'evaluating', retrieval_request_id = NULL, result_payload = NULL,
				    source_snapshots = '[]'::jsonb, error_message = NULL
				WHERE evaluation_id = $1 AND user_id = $2`, existingID, params.UserID); err != nil {
				return service.FeynmanEvaluation{}, false, fmt.Errorf("重试费曼评估失败: %w", err)
			}
			claimed = true
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO feynman_evaluations (
				evaluation_id, attempt_id, confirmation_id, rubric_id, knowledge_point_id,
				user_id, status, prompt_version, model_name, confirmed_transcript_hash, created_by
			) VALUES ($1, $2, $3, $4, $5, $6, 'evaluating', $7, $8, $9, $6)`,
			params.EvaluationID, params.AttemptID, params.ConfirmationID, params.RubricID,
			params.KnowledgePointID, params.UserID, params.PromptVersion, params.ModelName,
			params.ConfirmedTranscriptHash); err != nil {
			return service.FeynmanEvaluation{}, false, fmt.Errorf("创建费曼评估失败: %w", err)
		}
		existingID = params.EvaluationID
		claimed = true
	} else {
		return service.FeynmanEvaluation{}, false, fmt.Errorf("查询费曼评估失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return service.FeynmanEvaluation{}, false, fmt.Errorf("提交费曼评估事务失败: %w", err)
	}
	evaluation, err := r.GetEvaluationByID(ctx, params.UserID, existingID)
	return evaluation, claimed, err
}

func (r *PostgresFeynmanRepository) CompleteEvaluation(ctx context.Context, params service.CompleteFeynmanEvaluationParams) (service.FeynmanEvaluation, error) {
	var payloadJSON any
	var sourcesJSON any = []byte("[]")
	var err error
	if params.Payload != nil {
		payloadJSON, err = json.Marshal(params.Payload)
		if err != nil {
			return service.FeynmanEvaluation{}, err
		}
	}
	if params.Sources != nil {
		sourcesJSON, err = json.Marshal(params.Sources)
		if err != nil {
			return service.FeynmanEvaluation{}, err
		}
	}
	command, err := r.pool.Exec(ctx, `
		UPDATE feynman_evaluations
		SET status = $1, retrieval_request_id = NULLIF($2, '')::uuid,
		    result_payload = $3, source_snapshots = $4, error_message = NULLIF($5, '')
		WHERE evaluation_id = $6 AND user_id = $7 AND status = 'evaluating'`,
		params.Status, params.RetrievalRequestID, payloadJSON, sourcesJSON, params.ErrorMessage,
		params.EvaluationID, params.UserID)
	if err != nil {
		return service.FeynmanEvaluation{}, fmt.Errorf("完成费曼评估失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.FeynmanEvaluation{}, service.ErrFeynmanEvaluationNotReady
	}
	return r.GetEvaluationByID(ctx, params.UserID, params.EvaluationID)
}

func (r *PostgresFeynmanRepository) GetEvaluationByAttempt(ctx context.Context, userID, attemptID string) (service.FeynmanEvaluation, error) {
	return scanFeynmanEvaluation(r.pool.QueryRow(ctx, feynmanEvaluationSelectSQL+`
		WHERE e.attempt_id = $1 AND e.user_id = $2`, attemptID, userID))
}

func (r *PostgresFeynmanRepository) GetEvaluationByID(ctx context.Context, userID, evaluationID string) (service.FeynmanEvaluation, error) {
	return scanFeynmanEvaluation(r.pool.QueryRow(ctx, feynmanEvaluationSelectSQL+`
		WHERE e.evaluation_id = $1 AND e.user_id = $2`, evaluationID, userID))
}

func (r *PostgresFeynmanRepository) DecideEvaluation(ctx context.Context, params service.DecideFeynmanEvaluationParams) (service.FeynmanEvaluation, error) {
	var payloadJSON any
	var err error
	if params.FinalPayload != nil {
		payloadJSON, err = json.Marshal(params.FinalPayload)
		if err != nil {
			return service.FeynmanEvaluation{}, err
		}
	}
	command, err := r.pool.Exec(ctx, `
		INSERT INTO feynman_evaluation_decisions (
			decision_id, evaluation_id, user_id, decision, final_payload,
			decision_note, decided_by
		)
		SELECT $1, e.evaluation_id, e.user_id, $4, $5, NULLIF($6, ''), e.user_id
		FROM feynman_evaluations e
		WHERE e.evaluation_id = $2 AND e.user_id = $3 AND e.status = 'proposed'`,
		params.DecisionID, params.EvaluationID, params.UserID, params.Decision, payloadJSON, params.DecisionNote)
	if err != nil {
		if isUniqueViolation(err) {
			return service.FeynmanEvaluation{}, service.ErrFeynmanEvaluationDecided
		}
		return service.FeynmanEvaluation{}, fmt.Errorf("保存费曼评估决策失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.FeynmanEvaluation{}, service.ErrFeynmanEvaluationNotReady
	}
	return r.GetEvaluationByID(ctx, params.UserID, params.EvaluationID)
}
