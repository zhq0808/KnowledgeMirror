package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"KnowledgeMirror/internal/service"
)

// PostgresCoachRepository 持久化每日教练任务、分析快照、薄弱点和复习排程。
// 所有查询都显式带 user_id；分析提交是单事务，不暴露半份分析。
type PostgresCoachRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresCoachRepository(pool *pgxpool.Pool) *PostgresCoachRepository {
	return &PostgresCoachRepository{pool: pool}
}

const coachTaskSelectSQL = `
	SELECT coach_task_id::text, user_id, task_date, task_type, plan_role, status, source_key,
	       question_text, COALESCE(knowledge_point_id::text, ''), COALESCE(source_gap_id::text, ''),
	       COALESCE(source_review_id::text, ''), priority, COALESCE(session_id, ''), started_at, completed_at, created_at, updated_at
	FROM coach_daily_tasks`

func scanCoachTask(row rowScanner) (service.CoachDailyTask, error) {
	var task service.CoachDailyTask
	if err := row.Scan(
		&task.CoachTaskID, &task.UserID, &task.TaskDate, &task.TaskType, &task.PlanRole, &task.Status,
		&task.SourceKey, &task.QuestionText, &task.KnowledgePointID, &task.SourceGapID, &task.SourceReviewID,
		&task.Priority, &task.SessionID, &task.StartedAt, &task.CompletedAt,
		&task.CreatedAt, &task.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return task, service.ErrCoachTaskNotFound
		}
		return task, fmt.Errorf("扫描教练任务失败: %w", err)
	}
	return task, nil
}

func (r *PostgresCoachRepository) GetTask(ctx context.Context, userID, coachTaskID string) (service.CoachDailyTask, error) {
	return scanCoachTask(r.pool.QueryRow(ctx, coachTaskSelectSQL+`
		WHERE coach_task_id = $1 AND user_id = $2`, coachTaskID, userID))
}

func (r *PostgresCoachRepository) GetGap(ctx context.Context, userID, gapID string) (service.FeynmanGap, error) {
	var gap service.FeynmanGap
	err := r.pool.QueryRow(ctx, `
		SELECT gap_id::text, user_id, COALESCE(knowledge_point_id::text, ''), gap_key,
		       gap_type, diagnostic_dimension, title, description, status, evidence_count,
		       first_seen_at, last_seen_at, next_review_at,
		       (SELECT MIN(r.scheduled_date) FROM feynman_gap_reviews r
		        WHERE r.gap_id = feynman_gaps.gap_id AND r.user_id = feynman_gaps.user_id
		          AND r.status IN ('scheduled', 'missed')),
		       created_at, updated_at
		FROM feynman_gaps WHERE gap_id = $1 AND user_id = $2`, gapID, userID).Scan(
		&gap.GapID, &gap.UserID, &gap.KnowledgePointID, &gap.GapKey, &gap.GapType,
		&gap.DiagnosticDimension, &gap.Title, &gap.Description, &gap.Status, &gap.EvidenceCount,
		&gap.FirstSeenAt, &gap.LastSeenAt, &gap.NextReviewAt, &gap.NextReviewDate, &gap.CreatedAt, &gap.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return gap, service.ErrCoachTaskNotFound
	}
	if err != nil {
		return gap, fmt.Errorf("查询教练来源薄弱点失败: %w", err)
	}
	return gap, nil
}

// EnsureDailyPlan 在用户+日期 advisory lock 内生成“1 必做 + 最多 2 选做”。
// 排序固定为到期/逾期 review 优先，再按已确认 active knowledge point；
// 若用户已有 in_progress 任务，仅返回该任务，不创建竞争计划。
func (r *PostgresCoachRepository) EnsureDailyPlan(ctx context.Context, userID string, date time.Time) (service.CoachDailyPlan, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.CoachDailyPlan{}, fmt.Errorf("开启每日教练计划事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 与 StartTaskInSession 共用用户级锁，确保检查 active 与创建计划之间不会插入
	// 另一条 in_progress 任务；跨日期读取也不能生成竞争处方。
	lockKey := "coach-user:" + userID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return service.CoachDailyPlan{}, fmt.Errorf("锁定每日教练计划失败: %w", err)
	}

	active, found, err := findActiveCoachTaskTx(ctx, tx, userID)
	if err != nil {
		return service.CoachDailyPlan{}, err
	}
	if found {
		tasks, err := listCoachTasksByDateTx(ctx, tx, userID, date)
		if err != nil {
			return service.CoachDailyPlan{}, err
		}
		plan := buildCoachDailyPlan(date, tasks)
		plan.ActiveTask = &active
		if err := tx.Commit(ctx); err != nil {
			return service.CoachDailyPlan{}, fmt.Errorf("提交每日教练计划事务失败: %w", err)
		}
		return plan, nil
	}

	tasks, err := listCoachTasksByDateTx(ctx, tx, userID, date)
	if err != nil {
		return service.CoachDailyPlan{}, err
	}
	for len(tasks) < 3 {
		task, found, err := nextCoachPlanCandidateTx(ctx, tx, userID, date)
		if err != nil {
			return service.CoachDailyPlan{}, err
		}
		if !found {
			break
		}
		inserted, err := insertCoachPlanTaskTx(ctx, tx, userID, date, len(tasks), task)
		if err != nil {
			return service.CoachDailyPlan{}, err
		}
		if !inserted {
			return service.CoachDailyPlan{}, fmt.Errorf("创建每日教练任务失败: 候选未产生进展")
		}
		tasks, err = listCoachTasksByDateTx(ctx, tx, userID, date)
		if err != nil {
			return service.CoachDailyPlan{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return service.CoachDailyPlan{}, fmt.Errorf("提交每日教练计划事务失败: %w", err)
	}
	return buildCoachDailyPlan(date, tasks), nil
}

type coachPlanCandidate struct {
	TaskID           string
	TaskType         string
	SourceKey        string
	QuestionText     string
	KnowledgePointID string
	GapID            string
	ReviewID         string
}

func findActiveCoachTaskTx(ctx context.Context, tx pgx.Tx, userID string) (service.CoachDailyTask, bool, error) {
	task, err := scanCoachTask(tx.QueryRow(ctx, coachTaskSelectSQL+`
		WHERE user_id = $1 AND status IN ('in_progress', 'awaiting_retry')
		ORDER BY started_at, coach_task_id LIMIT 1`, userID))
	if errors.Is(err, service.ErrCoachTaskNotFound) {
		return service.CoachDailyTask{}, false, nil
	}
	if err != nil {
		return service.CoachDailyTask{}, false, err
	}
	return task, true, nil
}

func listCoachTasksByDateTx(ctx context.Context, tx pgx.Tx, userID string, date time.Time) ([]service.CoachDailyTask, error) {
	rows, err := tx.Query(ctx, coachTaskSelectSQL+`
		WHERE user_id = $1 AND task_date = $2
		ORDER BY priority, coach_task_id`, userID, date)
	if err != nil {
		return nil, fmt.Errorf("查询每日教练任务失败: %w", err)
	}
	defer rows.Close()
	tasks := make([]service.CoachDailyTask, 0, 3)
	for rows.Next() {
		task, err := scanCoachTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历每日教练任务失败: %w", err)
	}
	return tasks, nil
}

func nextCoachPlanCandidateTx(ctx context.Context, tx pgx.Tx, userID string, date time.Time) (coachPlanCandidate, bool, error) {
	candidate := coachPlanCandidate{}
	newID, err := service.NewCoachTaskID()
	if err != nil {
		return candidate, false, err
	}
	err = tx.QueryRow(ctx, `
		SELECT $3::text, 'feynman_retry', 'review:' || r.gap_review_id::text,
		       $4 || g.title,
		       COALESCE(g.knowledge_point_id::text, ''), g.gap_id::text, r.gap_review_id::text
		FROM feynman_gap_reviews r
		JOIN feynman_gaps g ON g.gap_id = r.gap_id AND g.user_id = r.user_id
		WHERE r.user_id = $1 AND r.status IN ('scheduled', 'missed') AND r.scheduled_date <= $2::date
		  AND g.status = 'open'
		  AND NOT EXISTS (
				SELECT 1 FROM coach_daily_tasks t
				WHERE t.source_review_id = r.gap_review_id
					  AND (t.status <> 'skipped' OR t.task_date = $2::date)
			  )
		  AND NOT EXISTS (
			SELECT 1 FROM feynman_gap_reviews earlier
			WHERE earlier.review_cycle_id = r.review_cycle_id
			  AND earlier.stage < r.stage AND earlier.status <> 'passed'
		  )
		ORDER BY r.scheduled_date, r.gap_review_id
		LIMIT 1`, userID, date, newID, service.CoachGapReviewQuestionPrefix).Scan(
		&candidate.TaskID, &candidate.TaskType, &candidate.SourceKey, &candidate.QuestionText,
		&candidate.KnowledgePointID, &candidate.GapID, &candidate.ReviewID)
	if err == nil {
		return candidate, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return candidate, false, fmt.Errorf("选择到期薄弱点复习失败: %w", err)
	}

	newID, err = service.NewCoachTaskID()
	if err != nil {
		return candidate, false, err
	}
	err = tx.QueryRow(ctx, `
		SELECT $3::text, 'feynman_new', 'knowledge:' || kp.knowledge_point_id::text,
		       $4 || kp.title, kp.knowledge_point_id::text, '', ''
		FROM knowledge_points kp
		WHERE kp.user_id = $1 AND kp.status = 'active' AND kp.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM coach_daily_tasks t
			WHERE t.user_id = kp.user_id AND t.knowledge_point_id = kp.knowledge_point_id
			  AND t.task_date = $2
		  )
		ORDER BY COALESCE((
			SELECT MAX(t2.task_date) FROM coach_daily_tasks t2
			WHERE t2.user_id = kp.user_id AND t2.knowledge_point_id = kp.knowledge_point_id
		), DATE '0001-01-01'), kp.created_at, kp.knowledge_point_id
		LIMIT 1`, userID, date, newID, service.CoachNewTopicQuestionPrefix).Scan(
		&candidate.TaskID, &candidate.TaskType, &candidate.SourceKey, &candidate.QuestionText,
		&candidate.KnowledgePointID, &candidate.GapID, &candidate.ReviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return candidate, false, nil
	}
	if err != nil {
		return candidate, false, fmt.Errorf("选择已确认知识点失败: %w", err)
	}
	return candidate, true, nil
}

func insertCoachPlanTaskTx(ctx context.Context, tx pgx.Tx, userID string, date time.Time, position int, candidate coachPlanCandidate) (bool, error) {
	role := service.CoachPlanRoleOptional
	if position == 0 {
		role = service.CoachPlanRoleRequired
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO coach_daily_tasks (
			coach_task_id, user_id, task_date, task_type, plan_role, status, source_key,
			question_text, knowledge_point_id, source_gap_id, source_review_id,
			priority, created_by, updated_by
		) VALUES (
			$1, $2, $3, $4, $5, 'pending', $6, $7,
			NULLIF($8, '')::uuid, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid,
			$11, $2, $2
		) ON CONFLICT (user_id, task_date, source_key) DO NOTHING`,
		candidate.TaskID, userID, date, candidate.TaskType, role, candidate.SourceKey,
		candidate.QuestionText, candidate.KnowledgePointID, candidate.GapID, candidate.ReviewID, position+1)
	if err != nil {
		return false, fmt.Errorf("创建每日教练任务失败: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

func buildCoachDailyPlan(date time.Time, tasks []service.CoachDailyTask) service.CoachDailyPlan {
	plan := service.CoachDailyPlan{
		Date:          date,
		Optional:      make([]service.CoachDailyTask, 0, 2),
		TerminalTasks: make([]service.CoachDailyTask, 0, len(tasks)),
	}
	for i := range tasks {
		task := tasks[i]
		if task.Status == service.CoachTaskStatusCompleted || task.Status == service.CoachTaskStatusSkipped {
			plan.TerminalTasks = append(plan.TerminalTasks, task)
			continue
		}
		if task.Status == service.CoachTaskStatusInProgress || task.Status == service.CoachTaskStatusAwaitingRetry {
			plan.ActiveTask = &task
		}
		if task.PlanRole == service.CoachPlanRoleRequired && plan.Required == nil {
			plan.Required = &task
		} else if task.PlanRole == service.CoachPlanRoleOptional {
			plan.Optional = append(plan.Optional, task)
		}
	}
	return plan
}

// StartTaskInSession 先锁任务再绑定会话，并在同一事务中建立教练来源的费曼状态。
// 已在同一会话启动的重放直接返回；其它状态或会话不允许抢占。
func (r *PostgresCoachRepository) StartTaskInSession(ctx context.Context, params service.StartCoachTaskParams) (service.CoachDailyTask, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.CoachDailyTask{}, fmt.Errorf("开启教练任务事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "coach-user:"+params.UserID); err != nil {
		return service.CoachDailyTask{}, fmt.Errorf("锁定用户教练任务失败: %w", err)
	}

	task, err := scanCoachTask(tx.QueryRow(ctx, coachTaskSelectSQL+`
		WHERE coach_task_id = $1 AND user_id = $2 FOR UPDATE`, params.CoachTaskID, params.UserID))
	if err != nil {
		return service.CoachDailyTask{}, err
	}
	if (task.Status == service.CoachTaskStatusInProgress || task.Status == service.CoachTaskStatusAwaitingRetry) && task.SessionID == params.SessionID {
		var stateTaskID, stateOrigin, stateQuestion, lastMessageID, lastFeedback string
		stateErr := tx.QueryRow(ctx, `
			SELECT COALESCE(coach_task_id::text, ''), question_origin, original_question_text,
				       COALESCE(last_answered_message_id::text, ''), last_feedback
			FROM feynman_practice_states
			WHERE session_id = $1 AND user_id = $2
			FOR UPDATE`, params.SessionID, params.UserID).Scan(&stateTaskID, &stateOrigin, &stateQuestion, &lastMessageID, &lastFeedback)
		if errors.Is(stateErr, pgx.ErrNoRows) {
			return service.CoachDailyTask{}, fmt.Errorf("%w: 活动任务缺少练习状态", service.ErrCoachTaskNotStartable)
		}
		if stateErr != nil {
			return service.CoachDailyTask{}, fmt.Errorf("读取教练练习状态失败: %w", stateErr)
		}
		if stateTaskID != task.CoachTaskID || stateOrigin != service.FeynmanQuestionOriginCoachTask || stateQuestion != task.QuestionText {
			return service.CoachDailyTask{}, fmt.Errorf("%w: 活动任务与练习状态不匹配", service.ErrCoachTaskNotStartable)
		}
		if params.UserMessageID != "" && (lastMessageID != params.UserMessageID || lastFeedback != params.Reply) {
			return service.CoachDailyTask{}, fmt.Errorf("%w: 启动消息与已处理结果不匹配", service.ErrCoachTaskNotStartable)
		}
		if err := tx.Commit(ctx); err != nil {
			return service.CoachDailyTask{}, fmt.Errorf("提交教练任务事务失败: %w", err)
		}
		return task, nil
	}
	if task.Status != service.CoachTaskStatusPending {
		return service.CoachDailyTask{}, service.ErrCoachTaskNotStartable
	}
	active, found, err := findActiveCoachTaskTx(ctx, tx, params.UserID)
	if err != nil {
		return service.CoachDailyTask{}, err
	}
	if found && active.CoachTaskID != task.CoachTaskID {
		return service.CoachDailyTask{}, fmt.Errorf("%w: 已有其它活动教练任务 %s", service.ErrCoachTaskNotStartable, active.CoachTaskID)
	}
	if task.SourceReviewID != "" {
		var reviewStatus string
		if err := tx.QueryRow(ctx, `
			SELECT status FROM feynman_gap_reviews
			WHERE gap_review_id = $1 AND user_id = $2 FOR UPDATE`, task.SourceReviewID, task.UserID).Scan(&reviewStatus); err != nil {
			return service.CoachDailyTask{}, fmt.Errorf("锁定任务来源复测失败: %w", err)
		}
		if reviewStatus != service.FeynmanGapReviewStatusScheduled && reviewStatus != service.FeynmanGapReviewStatusMissed {
			return service.CoachDailyTask{}, service.ErrCoachTaskNotStartable
		}
	}
	startedAt := params.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	command, err := tx.Exec(ctx, `
		UPDATE coach_daily_tasks
		SET status = 'in_progress', session_id = $3, started_at = $4, updated_by = user_id
		WHERE coach_task_id = $1 AND user_id = $2 AND status = 'pending'`,
		params.CoachTaskID, params.UserID, params.SessionID, startedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return service.CoachDailyTask{}, fmt.Errorf("%w: 用户已有活动教练任务", service.ErrCoachTaskNotStartable)
		}
		return service.CoachDailyTask{}, fmt.Errorf("启动教练任务失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.CoachDailyTask{}, fmt.Errorf("%w: 待启动任务状态已变化", service.ErrCoachTaskNotStartable)
	}

	command, err = tx.Exec(ctx, `
		INSERT INTO feynman_practice_states (
			session_id, user_id, state, active_question_text, question_origin,
			coach_task_id, original_question_text, retry_required,
			last_answered_message_id, last_feedback, round_no
		) VALUES ($1, $2, 'awaiting_answer', $3, 'coach_task', $4, $3, FALSE,
			NULLIF($5, '')::uuid, $6, 1)
		ON CONFLICT (session_id) DO UPDATE SET
			state = EXCLUDED.state,
			active_question_text = EXCLUDED.active_question_text,
			question_origin = EXCLUDED.question_origin,
			coach_task_id = EXCLUDED.coach_task_id,
			original_question_text = EXCLUDED.original_question_text,
			retry_required = FALSE,
			last_answered_message_id = EXCLUDED.last_answered_message_id,
			last_feedback = EXCLUDED.last_feedback,
			round_no = EXCLUDED.round_no
		WHERE feynman_practice_states.user_id = EXCLUDED.user_id
		  AND feynman_practice_states.state = 'idle'`,
		params.SessionID, params.UserID, task.QuestionText, params.CoachTaskID,
		params.UserMessageID, params.Reply)
	if err != nil {
		if isUniqueViolation(err) {
			return service.CoachDailyTask{}, fmt.Errorf("%w: 会话正在进行其它练习", service.ErrCoachTaskNotStartable)
		}
		return service.CoachDailyTask{}, fmt.Errorf("建立教练练习状态失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.CoachDailyTask{}, fmt.Errorf("%w: 会话正在进行其它练习", service.ErrCoachTaskNotStartable)
	}

	if err := tx.Commit(ctx); err != nil {
		return service.CoachDailyTask{}, fmt.Errorf("提交教练任务事务失败: %w", err)
	}
	return r.GetTask(ctx, params.UserID, params.CoachTaskID)
}

// ControlTask 在一个事务内推进 Coach 任务和匹配的练习投影。
func (r *PostgresCoachRepository) ControlTask(ctx context.Context, params service.CoachTaskControlParams) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启教练控制事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "coach-user:"+params.UserID); err != nil {
		return fmt.Errorf("锁定用户教练任务失败: %w", err)
	}
	var status, sessionID string
	if err := tx.QueryRow(ctx, `
		SELECT status, COALESCE(session_id, '') FROM coach_daily_tasks
		WHERE coach_task_id = $1 AND user_id = $2 FOR UPDATE`, params.CoachTaskID, params.UserID).Scan(&status, &sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return service.ErrCoachTaskNotFound
		}
		return fmt.Errorf("锁定教练任务失败: %w", err)
	}
	if sessionID != params.SessionID || (status != service.CoachTaskStatusInProgress && status != service.CoachTaskStatusAwaitingRetry) {
		return service.ErrCoachTaskNotStartable
	}
	if params.PracticeState.LastAnsweredMessageID != params.UserMessageID || params.PracticeState.LastFeedback != params.Reply {
		return fmt.Errorf("%w: 控制消息回放字段不匹配", service.ErrCoachAnalysisInput)
	}
	controlledAt := params.ControlledAt
	if controlledAt.IsZero() {
		controlledAt = time.Now()
	}
	switch params.Action {
	case "pause", "resume":
		if err := saveCoachPracticeStateTx(ctx, tx, params.PracticeState, params.CoachTaskID); err != nil {
			return err
		}
	case "skip", "stop":
		if status == service.CoachTaskStatusAwaitingRetry {
			var failedReview bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM coach_daily_tasks t
					JOIN feynman_gap_reviews r ON r.gap_review_id = t.source_review_id AND r.user_id = t.user_id
					WHERE t.coach_task_id = $1 AND t.user_id = $2 AND r.status = 'failed'
				)`, params.CoachTaskID, params.UserID).Scan(&failedReview); err != nil {
				return fmt.Errorf("检查复测纠正状态失败: %w", err)
			}
			if failedReview {
				return service.ErrCoachCorrectionPending
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE coach_daily_tasks SET status = 'skipped', completed_at = $3, updated_by = user_id
			WHERE coach_task_id = $1 AND user_id = $2`, params.CoachTaskID, params.UserID, controlledAt); err != nil {
			return fmt.Errorf("跳过教练任务失败: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE feynman_gap_reviews r
				SET status = 'missed', completed_at = $3, updated_by = r.user_id
			FROM coach_daily_tasks t
			WHERE t.coach_task_id = $1 AND t.user_id = $2
			  AND r.gap_review_id = t.source_review_id AND r.status IN ('scheduled', 'missed')`, params.CoachTaskID, params.UserID, controlledAt); err != nil {
			return fmt.Errorf("标记复测错过失败: %w", err)
		}
		if err := saveCoachPracticeStateTx(ctx, tx, params.PracticeState, params.CoachTaskID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: 未知控制动作 %q", service.ErrCoachAnalysisInput, params.Action)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交教练控制事务失败: %w", err)
	}
	return nil
}

// FetchPriorGapEvidence 批量读取归类所需的最小证据，不返回回答正文或完整分析 JSON。
func (r *PostgresCoachRepository) GetProgress(ctx context.Context, userID string, from, to time.Time) (service.CoachProgress, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT task_date,
		       COUNT(*) FILTER (WHERE plan_role = 'required'),
		       COUNT(*) FILTER (WHERE plan_role = 'required' AND status = 'completed'),
		       COUNT(*) FILTER (WHERE plan_role = 'optional'),
		       COUNT(*) FILTER (WHERE plan_role = 'optional' AND status = 'completed'),
		       COUNT(*) FILTER (WHERE status = 'pending'),
		       COUNT(*) FILTER (WHERE status = 'in_progress'),
		       COUNT(*) FILTER (WHERE status = 'awaiting_retry'),
		       COUNT(*) FILTER (WHERE status = 'completed'),
		       COUNT(*) FILTER (WHERE status = 'skipped')
		FROM coach_daily_tasks
		WHERE user_id = $1 AND task_date BETWEEN $2 AND $3
		GROUP BY task_date ORDER BY task_date`, userID, from, to)
	if err != nil {
		return service.CoachProgress{}, fmt.Errorf("查询教练进度失败: %w", err)
	}
	defer rows.Close()
	progress := service.CoachProgress{From: from, To: to, Days: make([]service.CoachProgressDay, 0)}
	for rows.Next() {
		var day service.CoachProgressDay
		if err := rows.Scan(&day.Date, &day.RequiredTotal, &day.RequiredCompleted,
			&day.OptionalTotal, &day.OptionalCompleted, &day.Pending, &day.InProgress,
			&day.AwaitingRetry, &day.Completed, &day.Skipped); err != nil {
			return service.CoachProgress{}, fmt.Errorf("扫描教练进度失败: %w", err)
		}
		progress.RequiredTotal += day.RequiredTotal
		progress.RequiredCompleted += day.RequiredCompleted
		progress.OptionalTotal += day.OptionalTotal
		progress.OptionalCompleted += day.OptionalCompleted
		progress.Pending += day.Pending
		progress.InProgress += day.InProgress
		progress.AwaitingRetry += day.AwaitingRetry
		progress.Completed += day.Completed
		progress.Skipped += day.Skipped
		progress.Days = append(progress.Days, day)
	}
	if err := rows.Err(); err != nil {
		return service.CoachProgress{}, fmt.Errorf("遍历教练进度失败: %w", err)
	}
	return progress, nil
}

func (r *PostgresCoachRepository) ListGaps(ctx context.Context, userID, status string, limit int) ([]service.FeynmanGap, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT gap_id::text, user_id, COALESCE(knowledge_point_id::text, ''), gap_key,
		       gap_type, diagnostic_dimension, title, description, status, evidence_count, first_seen_at,
		       last_seen_at, next_review_at,
		       (SELECT MIN(r.scheduled_date) FROM feynman_gap_reviews r
		        WHERE r.gap_id = feynman_gaps.gap_id AND r.user_id = feynman_gaps.user_id
		          AND r.status IN ('scheduled', 'missed')),
		       created_at, updated_at
		FROM feynman_gaps
		WHERE user_id = $1 AND status = $2
		ORDER BY CASE WHEN next_review_at IS NOT NULL AND next_review_at <= now() THEN 0 ELSE 1 END,
		         next_review_at NULLS LAST, last_seen_at DESC, gap_id
		LIMIT $3`, userID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("查询费曼薄弱点列表失败: %w", err)
	}
	defer rows.Close()
	gaps := make([]service.FeynmanGap, 0, limit)
	for rows.Next() {
		var gap service.FeynmanGap
		if err := rows.Scan(&gap.GapID, &gap.UserID, &gap.KnowledgePointID, &gap.GapKey,
			&gap.GapType, &gap.DiagnosticDimension, &gap.Title, &gap.Description, &gap.Status, &gap.EvidenceCount,
			&gap.FirstSeenAt, &gap.LastSeenAt, &gap.NextReviewAt, &gap.NextReviewDate, &gap.CreatedAt, &gap.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描费曼薄弱点失败: %w", err)
		}
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历费曼薄弱点失败: %w", err)
	}
	return gaps, nil
}

func (r *PostgresCoachRepository) FetchPriorGapEvidence(ctx context.Context, userID string, gapKeys []string) (map[string]service.PriorGapEvidence, error) {
	result := make(map[string]service.PriorGapEvidence, len(gapKeys))
	if len(gapKeys) == 0 {
		return result, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT g.gap_id::text, g.gap_key, g.gap_type, g.status, g.evidence_count, g.last_seen_at,
		       COALESCE(last_evidence.coach_attempt_id::text, ''), COALESCE(last_evidence.severity, 0),
		       EXISTS (
			   SELECT 1 FROM coach_attempts passed
			   WHERE passed.user_id = g.user_id AND passed.outcome = 'passed'
		       )
		FROM feynman_gaps g
		LEFT JOIN LATERAL (
			SELECT ca.coach_attempt_id, cag.severity
			FROM coach_attempt_gaps cag
			JOIN coach_attempts ca
			  ON ca.coach_attempt_id = cag.coach_attempt_id AND ca.user_id = cag.user_id
			WHERE cag.gap_id = g.gap_id AND cag.user_id = g.user_id
			ORDER BY cag.created_at DESC, cag.attempt_gap_id DESC
			LIMIT 1
		) last_evidence ON TRUE
		WHERE g.user_id = $1 AND g.gap_key = ANY($2::varchar[])`, userID, gapKeys)
	if err != nil {
		return nil, fmt.Errorf("查询薄弱点历史证据失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var evidence service.PriorGapEvidence
		if err := rows.Scan(&evidence.GapID, &evidence.GapKey, &evidence.GapType, &evidence.Status,
			&evidence.EvidenceCount, &evidence.LastSeenAt, &evidence.LastAttemptID, &evidence.LastSeverity,
			&evidence.SuccessfulOutput); err != nil {
			return nil, fmt.Errorf("扫描薄弱点历史证据失败: %w", err)
		}
		result[evidence.GapKey] = evidence
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历薄弱点历史证据失败: %w", err)
	}
	return result, nil
}

// CommitAnalysis 原子提交完整分析。answer_message_id 的唯一约束承担请求幂等；
// 若发生并发重放，返回已存在且同属当前用户/任务的尝试。
func (r *PostgresCoachRepository) CommitAnalysis(ctx context.Context, params service.CommitCoachAnalysisParams) (service.CoachAttempt, error) {
	if err := service.ValidateCoachAnalysisCommit(&params); err != nil {
		return service.CoachAttempt{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return service.CoachAttempt{}, fmt.Errorf("开启教练分析事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var taskQuestion, taskStatus, taskSession, sourceGapID, sourceReviewID string
	err = tx.QueryRow(ctx, `
		SELECT question_text, status, COALESCE(session_id, ''),
			       COALESCE(source_gap_id::text, ''), COALESCE(source_review_id::text, '')
		FROM coach_daily_tasks
		WHERE coach_task_id = $1 AND user_id = $2
		FOR UPDATE`, params.Attempt.CoachTaskID, params.Attempt.UserID).
		Scan(&taskQuestion, &taskStatus, &taskSession, &sourceGapID, &sourceReviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return service.CoachAttempt{}, service.ErrCoachTaskNotFound
	}
	if err != nil {
		return service.CoachAttempt{}, fmt.Errorf("锁定教练任务失败: %w", err)
	}
	existing, findErr := getCoachAttemptByAnswerTx(ctx, tx, params.Attempt.UserID, params.Attempt.AnswerMessageID)
	if findErr == nil {
		if existing.CoachTaskID != params.Attempt.CoachTaskID {
			return service.CoachAttempt{}, service.ErrCoachAttemptConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return service.CoachAttempt{}, fmt.Errorf("提交教练分析幂等事务失败: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(findErr, pgx.ErrNoRows) {
		return service.CoachAttempt{}, fmt.Errorf("查询教练分析幂等记录失败: %w", findErr)
	}
	if (taskStatus != service.CoachTaskStatusInProgress && taskStatus != service.CoachTaskStatusAwaitingRetry) || taskSession != params.Attempt.SessionID || taskQuestion != params.Attempt.OriginalQuestionText {
		return service.CoachAttempt{}, fmt.Errorf("%w: 任务状态、会话或原题快照不匹配", service.ErrCoachAnalysisInput)
	}
	if params.PracticeState.UserID != params.Attempt.UserID || params.PracticeState.SessionID != params.Attempt.SessionID {
		return service.CoachAttempt{}, fmt.Errorf("%w: 练习状态归属不匹配", service.ErrCoachAnalysisInput)
	}
	if params.CorrectionDate.IsZero() {
		return service.CoachAttempt{}, fmt.Errorf("%w: 缺少回答本地日期", service.ErrCoachAnalysisInput)
	}
	isRetest := sourceGapID != "" && sourceReviewID != ""
	if params.ReviewDecision.IsRetest != isRetest {
		return service.CoachAttempt{}, fmt.Errorf("%w: 复测任务身份不匹配", service.ErrCoachAnalysisInput)
	}
	targetRecurred := false
	for _, gap := range params.Gaps {
		if gap.ForceCanonicalGapID != "" && gap.ForceCanonicalGapID == sourceGapID {
			targetRecurred = true
			break
		}
	}
	if isRetest && taskStatus == service.CoachTaskStatusInProgress &&
		(params.ReviewDecision.TargetRecurred != targetRecurred ||
			(params.ReviewDecision.CurrentReviewStatus == service.FeynmanGapReviewStatusFailed) != targetRecurred) {
		return service.CoachAttempt{}, fmt.Errorf("%w: 复测结果与目标薄弱点证据不一致", service.ErrCoachAnalysisInput)
	}
	if isRetest && taskStatus == service.CoachTaskStatusAwaitingRetry && params.ReviewDecision.TargetRecurred {
		return service.CoachAttempt{}, fmt.Errorf("%w: 纠正通过阶段不能再次完成当前复测", service.ErrCoachAnalysisInput)
	}
	if params.Attempt.Outcome == service.CoachAttemptOutcomeRetryRequired &&
		(params.PracticeState.CoachTaskID != params.Attempt.CoachTaskID || params.PracticeState.QuestionOrigin != service.FeynmanQuestionOriginCoachTask ||
			params.PracticeState.OriginalQuestionText != taskQuestion) {
		return service.CoachAttempt{}, fmt.Errorf("%w: 重答练习状态或题目快照不匹配", service.ErrCoachAnalysisInput)
	}
	if params.Attempt.Outcome == service.CoachAttemptOutcomePassed && params.PracticeState.State != service.FeynmanStateIdle {
		return service.CoachAttempt{}, fmt.Errorf("%w: 通过后练习状态必须为空闲", service.ErrCoachAnalysisInput)
	}

	attempt, err := insertCoachAttempt(ctx, tx, params.Attempt)
	if err != nil {
		if isUniqueViolation(err) {
			return service.CoachAttempt{}, service.ErrCoachAttemptConflict
		}
		return service.CoachAttempt{}, err
	}

	seenAt := params.CompletedAt
	if seenAt.IsZero() {
		seenAt = time.Now()
	}
	canonicalGapIDs := make(map[string]string, len(params.Gaps))
	for _, gap := range params.Gaps {
		canonicalGapID, classification, err := upsertCoachGap(
			ctx, tx, params.Attempt.UserID, params.Attempt.CoachTaskID, gap, seenAt,
		)
		if err != nil {
			return service.CoachAttempt{}, err
		}
		canonicalGapIDs[gap.GapID] = canonicalGapID
		if _, err := tx.Exec(ctx, `
			INSERT INTO coach_attempt_gaps (
				attempt_gap_id, coach_attempt_id, gap_id, user_id, gap_key, gap_type,
				diagnostic_dimension, classification, title, description, severity, is_focus, evidence_payload
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			gap.AttemptGapID, params.Attempt.CoachAttemptID, canonicalGapID, params.Attempt.UserID,
			gap.GapKey, gap.GapType, gap.DiagnosticDimension, classification, gap.Title, gap.Description,
			gap.Severity, gap.IsFocus, []byte(gap.EvidenceJSON)); err != nil {
			return service.CoachAttempt{}, fmt.Errorf("保存教练尝试薄弱点失败: %w", err)
		}
	}

	if err := applyCoachReviewLifecycleTx(
		ctx, tx, params, taskStatus, sourceGapID, sourceReviewID, canonicalGapIDs, seenAt,
	); err != nil {
		return service.CoachAttempt{}, err
	}

	completedAt := params.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	command, err := tx.Exec(ctx, `
		UPDATE coach_daily_tasks
		SET status = CASE WHEN $4 = 'passed' THEN 'completed' ELSE 'awaiting_retry' END,
		    completed_at = CASE WHEN $4 = 'passed' THEN $3 ELSE NULL END,
		    updated_by = user_id
		WHERE coach_task_id = $1 AND user_id = $2 AND status IN ('in_progress', 'awaiting_retry')`,
		params.Attempt.CoachTaskID, params.Attempt.UserID, completedAt, params.Attempt.Outcome)
	if err != nil {
		return service.CoachAttempt{}, fmt.Errorf("完成教练任务失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return service.CoachAttempt{}, service.ErrCoachTaskNotStartable
	}

	if err := saveCoachPracticeStateTx(ctx, tx, params.PracticeState, params.Attempt.CoachTaskID); err != nil {
		return service.CoachAttempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return service.CoachAttempt{}, fmt.Errorf("提交教练分析事务失败: %w", err)
	}
	return attempt, nil
}

func applyCoachReviewLifecycleTx(
	ctx context.Context,
	tx pgx.Tx,
	params service.CommitCoachAnalysisParams,
	taskStatus, sourceGapID, sourceReviewID string,
	canonicalGapIDs map[string]string,
	completedAt time.Time,
) error {
	// The unaided first answer of a retest decides the target review independently
	// from any different blocking gaps found in the same answer.
	if sourceReviewID != "" && taskStatus == service.CoachTaskStatusInProgress {
		var reviewGapID, cycleID, currentStatus string
		var stage int
		if err := tx.QueryRow(ctx, `
			SELECT gap_id::text, review_cycle_id::text, stage, status
			FROM feynman_gap_reviews
			WHERE gap_review_id = $1 AND user_id = $2
			FOR UPDATE`, sourceReviewID, params.Attempt.UserID).
			Scan(&reviewGapID, &cycleID, &stage, &currentStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 复测来源不存在", service.ErrCoachAnalysisInput)
			}
			return fmt.Errorf("锁定当前复测失败: %w", err)
		}
		if reviewGapID != sourceGapID || (currentStatus != service.FeynmanGapReviewStatusScheduled && currentStatus != service.FeynmanGapReviewStatusMissed) {
			return fmt.Errorf("%w: 复测来源薄弱点或状态不匹配", service.ErrCoachAnalysisInput)
		}
		status := params.ReviewDecision.CurrentReviewStatus
		if _, err := tx.Exec(ctx, `
			UPDATE feynman_gap_reviews
			SET status = $3, completed_attempt_id = $4, completed_at = $5, updated_by = user_id
			WHERE gap_review_id = $1 AND user_id = $2`,
			sourceReviewID, params.Attempt.UserID, status, params.Attempt.CoachAttemptID, completedAt); err != nil {
			return fmt.Errorf("记录当前复测结果失败: %w", err)
		}
		if status == service.FeynmanGapReviewStatusPassed && stage == 3 {
			var laterActive bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM feynman_gap_reviews
					WHERE gap_id = $1 AND user_id = $2
					  AND status IN ('scheduled', 'missed')
				)`, sourceGapID, params.Attempt.UserID).Scan(&laterActive); err != nil {
				return fmt.Errorf("检查后续复测阶段失败: %w", err)
			}
			if !laterActive {
				if _, err := tx.Exec(ctx, `
					UPDATE feynman_gaps SET status = 'resolved', next_review_at = NULL, updated_by = user_id
					WHERE gap_id = $1 AND user_id = $2`, sourceGapID, params.Attempt.UserID); err != nil {
					return fmt.Errorf("解决复测薄弱点失败: %w", err)
				}
			}
		}
	}

	if params.Attempt.Outcome == service.CoachAttemptOutcomeRetryRequired {
		if sourceReviewID != "" && params.ReviewDecision.TargetRecurred {
			var oldCycleID string
			if err := tx.QueryRow(ctx, `
				SELECT review_cycle_id::text FROM feynman_gap_reviews
				WHERE gap_review_id = $1 AND user_id = $2`, sourceReviewID, params.Attempt.UserID).Scan(&oldCycleID); err != nil {
				return fmt.Errorf("读取失败复测周期失败: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE coach_daily_tasks t
				SET status = 'skipped', completed_at = $3, updated_by = t.user_id
				FROM feynman_gap_reviews r
				WHERE r.review_cycle_id = $1 AND r.user_id = $2
				  AND t.source_review_id = r.gap_review_id
				  AND t.coach_task_id <> $4 AND t.status = 'pending'`,
				oldCycleID, params.Attempt.UserID, completedAt, params.Attempt.CoachTaskID); err != nil {
				return fmt.Errorf("跳过旧周期后续任务失败: %w", err)
			}
		}
		for _, gap := range params.Gaps {
			if !gap.RequiresCorrection {
				continue
			}
			canonicalGapID := canonicalGapIDs[gap.GapID]
			if canonicalGapID == "" {
				return fmt.Errorf("%w: 待纠正薄弱点缺少 canonical identity", service.ErrCoachAnalysisInput)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO coach_task_pending_gaps (
					coach_task_id, gap_id, user_id, detected_attempt_id, status
				) VALUES ($1, $2, $3, $4, 'pending')
				ON CONFLICT (coach_task_id, gap_id) DO UPDATE SET
					detected_attempt_id = EXCLUDED.detected_attempt_id,
					corrected_attempt_id = NULL,
					status = 'pending'`,
				params.Attempt.CoachTaskID, canonicalGapID, params.Attempt.UserID, params.Attempt.CoachAttemptID); err != nil {
				return fmt.Errorf("保存任务待纠正薄弱点失败: %w", err)
			}
		}
		return refreshCoachGapNextReviewTx(ctx, tx, params.Attempt.UserID, sourceGapID)
	}

	// A passing answer only creates cycles when it is an immediate correction with
	// persisted pending rows. A first-pass new topic therefore creates no reviews.
	if taskStatus != service.CoachTaskStatusAwaitingRetry {
		return refreshCoachGapNextReviewTx(ctx, tx, params.Attempt.UserID, sourceGapID)
	}
	type pendingGap struct {
		gapID             string
		detectedAttemptID string
	}
	rows, err := tx.Query(ctx, `
		SELECT gap_id::text, detected_attempt_id::text
		FROM coach_task_pending_gaps
		WHERE coach_task_id = $1 AND user_id = $2 AND status = 'pending'
		ORDER BY gap_id
		FOR UPDATE`, params.Attempt.CoachTaskID, params.Attempt.UserID)
	if err != nil {
		return fmt.Errorf("锁定任务待纠正薄弱点失败: %w", err)
	}
	pending := make([]pendingGap, 0)
	for rows.Next() {
		var item pendingGap
		if err := rows.Scan(&item.gapID, &item.detectedAttemptID); err != nil {
			rows.Close()
			return fmt.Errorf("扫描任务待纠正薄弱点失败: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("遍历任务待纠正薄弱点失败: %w", err)
	}
	rows.Close()

	if sourceReviewID != "" {
		var oldCycleID, currentStatus string
		if err := tx.QueryRow(ctx, `
			SELECT review_cycle_id::text, status
			FROM feynman_gap_reviews
			WHERE gap_review_id = $1 AND user_id = $2
			FOR UPDATE`, sourceReviewID, params.Attempt.UserID).Scan(&oldCycleID, &currentStatus); err != nil {
			return fmt.Errorf("锁定失败复测周期失败: %w", err)
		}
		if currentStatus == service.FeynmanGapReviewStatusFailed {
			if _, err := tx.Exec(ctx, `
				UPDATE feynman_gap_reviews
				SET status = 'cancelled', completed_at = $3, updated_by = user_id
				WHERE review_cycle_id = $1 AND user_id = $2
				  AND status IN ('scheduled', 'missed')`, oldCycleID, params.Attempt.UserID, completedAt); err != nil {
				return fmt.Errorf("取消旧复测周期后续阶段失败: %w", err)
			}
		}
	}

	dates := service.FixedCoachReviewDates(params.CorrectionDate)
	for _, item := range pending {
		cycleID, err := service.NewFeynmanGapReviewCycleID()
		if err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `
			INSERT INTO feynman_gap_review_cycles (
				review_cycle_id, gap_id, source_attempt_id, correction_attempt_id,
				coach_task_id, user_id, anchor_date, created_by, updated_by
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $6, $6)
			ON CONFLICT (gap_id, correction_attempt_id) DO NOTHING`,
			cycleID, item.gapID, item.detectedAttemptID, params.Attempt.CoachAttemptID,
			params.Attempt.CoachTaskID, params.Attempt.UserID, params.CorrectionDate.Format(time.DateOnly))
		if err != nil {
			return fmt.Errorf("创建薄弱点复测周期失败: %w", err)
		}
		if command.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `
				SELECT review_cycle_id::text FROM feynman_gap_review_cycles
				WHERE gap_id = $1 AND correction_attempt_id = $2 AND user_id = $3`,
				item.gapID, params.Attempt.CoachAttemptID, params.Attempt.UserID).Scan(&cycleID); err != nil {
				return fmt.Errorf("读取幂等复测周期失败: %w", err)
			}
		}
		for index, scheduledDate := range dates {
			reviewID, err := service.NewFeynmanGapReviewID()
			if err != nil {
				return err
			}
			stage := index + 1
			if _, err := tx.Exec(ctx, `
				INSERT INTO feynman_gap_reviews (
					gap_review_id, review_cycle_id, gap_id, source_attempt_id, user_id,
					stage, scheduled_date, scheduled_for, status, created_by, updated_by
				) VALUES ($1, $2, $3, $4, $5, $6, $7::date, $7::date::timestamp, 'scheduled', $5, $5)
				ON CONFLICT (review_cycle_id, stage) DO NOTHING`,
				reviewID, cycleID, item.gapID, item.detectedAttemptID, params.Attempt.UserID,
				stage, scheduledDate.Format(time.DateOnly)); err != nil {
				return fmt.Errorf("创建固定复测阶段失败: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE coach_task_pending_gaps
			SET status = 'corrected', corrected_attempt_id = $4
			WHERE coach_task_id = $1 AND gap_id = $2 AND user_id = $3 AND status = 'pending'`,
			params.Attempt.CoachTaskID, item.gapID, params.Attempt.UserID, params.Attempt.CoachAttemptID); err != nil {
			return fmt.Errorf("完成任务薄弱点纠正失败: %w", err)
		}
		if err := refreshCoachGapNextReviewTx(ctx, tx, params.Attempt.UserID, item.gapID); err != nil {
			return err
		}
	}
	return refreshCoachGapNextReviewTx(ctx, tx, params.Attempt.UserID, sourceGapID)
}

func refreshCoachGapNextReviewTx(ctx context.Context, tx pgx.Tx, userID, gapID string) error {
	if gapID == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE feynman_gaps g
		SET next_review_at = (
			SELECT (MIN(r.scheduled_date)::timestamp AT TIME ZONE 'UTC')
			FROM feynman_gap_reviews r
			WHERE r.gap_id = g.gap_id AND r.user_id = g.user_id
			  AND r.status IN ('scheduled', 'missed')
		), updated_by = g.user_id
		WHERE g.gap_id = $1 AND g.user_id = $2`, gapID, userID); err != nil {
		return fmt.Errorf("刷新薄弱点下次复测日期失败: %w", err)
	}
	return nil
}

func insertCoachAttempt(ctx context.Context, tx pgx.Tx, attempt service.CoachAttempt) (service.CoachAttempt, error) {
	return scanCoachAttempt(tx.QueryRow(ctx, `
		INSERT INTO coach_attempts (
			coach_attempt_id, coach_task_id, user_id, session_id, answer_message_id,
			original_question_text, analysis_payload, outcome, prompt_version, model_name, created_by
		) SELECT $1, t.coach_task_id, t.user_id, $4, m.message_id, $6, $7, $8, $9, $10, t.user_id
		FROM coach_daily_tasks t
		JOIN agent_memory_episodic m
		  ON m.message_id = $5 AND m.user_id = t.user_id AND m.session_id = $4
		 AND m.role = 'user' AND m.status = 'completed' AND m.deleted_at IS NULL
		WHERE t.coach_task_id = $2 AND t.user_id = $3
		RETURNING coach_attempt_id::text, coach_task_id::text, user_id, session_id,
		          answer_message_id::text, original_question_text, analysis_payload,
		          outcome, prompt_version, model_name, created_at`,
		attempt.CoachAttemptID, attempt.CoachTaskID, attempt.UserID, attempt.SessionID,
		attempt.AnswerMessageID, attempt.OriginalQuestionText, []byte(attempt.AnalysisJSON),
		attempt.Outcome, attempt.PromptVersion, attempt.ModelName))
}

func getCoachAttemptByAnswerTx(ctx context.Context, tx pgx.Tx, userID, answerMessageID string) (service.CoachAttempt, error) {
	return scanCoachAttempt(tx.QueryRow(ctx, `
		SELECT coach_attempt_id::text, coach_task_id::text, user_id, session_id,
		       answer_message_id::text, original_question_text, analysis_payload,
		       outcome, prompt_version, model_name, created_at
		FROM coach_attempts WHERE answer_message_id = $1 AND user_id = $2`, answerMessageID, userID))
}

func scanCoachAttempt(row rowScanner) (service.CoachAttempt, error) {
	var attempt service.CoachAttempt
	var analysis []byte
	if err := row.Scan(&attempt.CoachAttemptID, &attempt.CoachTaskID, &attempt.UserID, &attempt.SessionID,
		&attempt.AnswerMessageID, &attempt.OriginalQuestionText, &analysis, &attempt.Outcome,
		&attempt.PromptVersion, &attempt.ModelName, &attempt.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return attempt, pgx.ErrNoRows
		}
		return attempt, fmt.Errorf("扫描教练尝试失败: %w", err)
	}
	attempt.AnalysisJSON = json.RawMessage(analysis)
	return attempt, nil
}

func upsertCoachGap(
	ctx context.Context,
	tx pgx.Tx,
	userID, coachTaskID string,
	gap service.CoachGapEvidence,
	seenAt time.Time,
) (canonicalGapID, classification string, err error) {
	var knowledgePointID string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(knowledge_point_id::text, '') FROM coach_daily_tasks
		WHERE coach_task_id = $1 AND user_id = $2`, coachTaskID, userID).Scan(&knowledgePointID); err != nil {
		return "", "", fmt.Errorf("读取教练任务知识点失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"coach-gap:"+userID+":"+gap.GapKey); err != nil {
		return "", "", fmt.Errorf("锁定费曼薄弱点失败: %w", err)
	}

	if gap.ForceCanonicalGapID != "" {
		err = tx.QueryRow(ctx, `
			SELECT gap_id::text FROM feynman_gaps
			WHERE gap_id = $1 AND user_id = $2 FOR UPDATE`, gap.ForceCanonicalGapID, userID).Scan(&canonicalGapID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("%w: 复测目标薄弱点不存在", service.ErrCoachAnalysisInput)
		}
	} else {
		err = tx.QueryRow(ctx, `
			SELECT gap_id::text
			FROM feynman_gaps
			WHERE user_id = $1 AND gap_key = $2`, userID, gap.GapKey).Scan(&canonicalGapID)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		canonicalGapID, err = service.NewFeynmanGapID()
		if err != nil {
			return "", "", err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO feynman_gaps (
				gap_id, user_id, knowledge_point_id, gap_key, gap_type, diagnostic_dimension, title, description,
				status, evidence_count, first_seen_at, last_seen_at, created_by, updated_by
			) VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, $7, $8, 'open', 1, $9, $9, $2, $2)`,
			canonicalGapID, userID, knowledgePointID, gap.GapKey, gap.GapType, gap.DiagnosticDimension,
			gap.Title, gap.Description, seenAt); err != nil {
			return "", "", fmt.Errorf("创建费曼薄弱点失败: %w", err)
		}
		return canonicalGapID, service.CoachGapClassificationNew, nil
	case err != nil:
		return "", "", fmt.Errorf("读取费曼薄弱点失败: %w", err)
	}
	if gap.ForceCanonicalGapID != "" && canonicalGapID != gap.ForceCanonicalGapID {
		return "", "", fmt.Errorf("%w: 复测目标薄弱点身份不匹配", service.ErrCoachAnalysisInput)
	}

	command, err := tx.Exec(ctx, `
		UPDATE feynman_gaps
		SET gap_type = $4,
			diagnostic_dimension = $5,
			title = $6,
			description = $7,
			status = 'open',
			evidence_count = evidence_count + 1,
			last_seen_at = $8,
			updated_by = user_id
		WHERE gap_id = $1 AND user_id = $2
		  AND (knowledge_point_id IS NOT DISTINCT FROM NULLIF($3, '')::uuid OR $9 <> '')`,
		canonicalGapID, userID, knowledgePointID, gap.GapType, gap.DiagnosticDimension, gap.Title, gap.Description, seenAt, gap.ForceCanonicalGapID)
	if err != nil {
		return "", "", fmt.Errorf("聚合费曼薄弱点失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return "", "", fmt.Errorf("%w: 薄弱点键与既有知识点冲突 %q", service.ErrCoachAnalysisInput, gap.GapKey)
	}
	return canonicalGapID, service.CoachGapClassificationRecurrent, nil
}

func saveCoachPracticeStateTx(ctx context.Context, tx pgx.Tx, state service.FeynmanPracticeState, expectedCoachTaskID string) error {
	command, err := tx.Exec(ctx, `
		UPDATE feynman_practice_states
		SET state = $3, active_question_text = $4, question_origin = $5,
		    coach_task_id = NULLIF($6, '')::uuid, original_question_text = $7,
		    retry_required = $8, last_answered_message_id = NULLIF($9, '')::uuid,
		    last_feedback = $10, round_no = $11
		WHERE session_id = $1 AND user_id = $2 AND coach_task_id = NULLIF($12, '')::uuid`,
		state.SessionID, state.UserID, state.State, state.ActiveQuestionText, state.QuestionOrigin,
		state.CoachTaskID, state.OriginalQuestionText, state.RetryRequired,
		state.LastAnsweredMessageID, state.LastFeedback, state.RoundNo, expectedCoachTaskID)
	if err != nil {
		return fmt.Errorf("推进教练练习状态失败: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("推进教练练习状态失败: 当前会话任务不匹配")
	}
	return nil
}
