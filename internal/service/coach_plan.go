package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	CoachNewTopicQuestionPrefix  = "请在不看资料的情况下，用 2–3 分钟完整讲清楚："
	CoachGapReviewQuestionPrefix = "请在不看资料的情况下，重新完整讲清楚这个薄弱点："
	CoachEmptyNoSources          = "no_learning_sources"
	CoachEmptyActionReview       = "review_candidates"
	CoachMaxProgressDays         = 90
	CoachDefaultGapLimit         = 50
	CoachMaxGapLimit             = 100
)

// CoachDailyPlan 是 Dashboard/Todo 共用的每日计划读模型。
type CoachDailyPlan struct {
	Date          time.Time
	Required      *CoachDailyTask
	Optional      []CoachDailyTask
	ActiveTask    *CoachDailyTask
	TerminalTasks []CoachDailyTask
	EmptyState    *CoachEmptyState
}

// CoachEmptyState 在没有合法来源时给出下一步，不伪造练习题。
type CoachEmptyState struct {
	Code       string
	Message    string
	Action     string
	ActionPath string
}

// CoachProgressDay 是一天的任务状态聚合。状态计数直接沿用任务表枚举。
type CoachProgressDay struct {
	Date              time.Time
	RequiredTotal     int
	RequiredCompleted int
	OptionalTotal     int
	OptionalCompleted int
	Pending           int
	InProgress        int
	AwaitingRetry     int
	Completed         int
	Skipped           int
}

// CoachProgress 是不超过 90 天的计划完成度。
type CoachProgress struct {
	From              time.Time
	To                time.Time
	RequiredTotal     int
	RequiredCompleted int
	OptionalTotal     int
	OptionalCompleted int
	Pending           int
	InProgress        int
	AwaitingRetry     int
	Completed         int
	Skipped           int
	Days              []CoachProgressDay
}

// CoachService 只负责编排读时计划生成与查询校验；不处理费曼回答。
type CoachService struct {
	repo CoachPlanRepository
	now  func() time.Time
}

func NewCoachService(repo CoachPlanRepository, now func() time.Time) *CoachService {
	if now == nil {
		now = time.Now
	}
	return &CoachService{repo: repo, now: now}
}

// Today 幂等确保并返回指定本地日期的计划。允许当前日期前后各一天，兼容客户端与服务端跨午夜。
func (s *CoachService) Today(ctx context.Context, userID, dateValue string) (CoachDailyPlan, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return CoachDailyPlan{}, fmt.Errorf("%w: 用户身份缺失", ErrCoachQueryInput)
	}
	now := s.now()
	date := localDate(now)
	if strings.TrimSpace(dateValue) != "" {
		var err error
		date, err = parseRequiredCoachDate("date", dateValue, now.Location())
		if err != nil {
			return CoachDailyPlan{}, err
		}
	}
	today := localDate(now)
	if daysBetween(today, date) < -1 || daysBetween(today, date) > 1 {
		return CoachDailyPlan{}, fmt.Errorf("%w: date 只能是当前日期前后一天", ErrCoachQueryInput)
	}
	plan, err := s.repo.EnsureDailyPlan(ctx, userID, date)
	if err != nil {
		return CoachDailyPlan{}, fmt.Errorf("确保每日教练计划失败: %w", err)
	}
	if plan.Required == nil && len(plan.Optional) == 0 && plan.ActiveTask == nil {
		plan.EmptyState = &CoachEmptyState{
			Code:       CoachEmptyNoSources,
			Message:    "暂无可安排的教练任务。请先确认至少一个有效知识点，或等待薄弱点复习到期。",
			Action:     CoachEmptyActionReview,
			ActionPath: "/knowledge",
		}
	}
	return plan, nil
}

// Progress 返回闭区间 [from,to] 的完成度；最多 90 个自然日。
func (s *CoachService) Progress(ctx context.Context, userID, fromValue, toValue string) (CoachProgress, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return CoachProgress{}, fmt.Errorf("%w: 用户身份缺失", ErrCoachQueryInput)
	}
	location := s.now().Location()
	from, err := parseRequiredCoachDate("from", fromValue, location)
	if err != nil {
		return CoachProgress{}, err
	}
	to, err := parseRequiredCoachDate("to", toValue, location)
	if err != nil {
		return CoachProgress{}, err
	}
	days := daysBetween(from, to)
	if days < 0 {
		return CoachProgress{}, fmt.Errorf("%w: from 不能晚于 to", ErrCoachQueryInput)
	}
	if days+1 > CoachMaxProgressDays {
		return CoachProgress{}, fmt.Errorf("%w: 查询范围不能超过 %d 天", ErrCoachQueryInput, CoachMaxProgressDays)
	}
	progress, err := s.repo.GetProgress(ctx, userID, from, to)
	if err != nil {
		return CoachProgress{}, fmt.Errorf("查询教练进度失败: %w", err)
	}
	return progress, nil
}

// Gaps 返回薄弱点当前投影。默认 open/50，limit 上限 100。
func (s *CoachService) Gaps(ctx context.Context, userID, statusValue, limitValue string) ([]FeynmanGap, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: 用户身份缺失", ErrCoachQueryInput)
	}
	status := strings.TrimSpace(statusValue)
	if status == "" {
		status = FeynmanGapStatusOpen
	}
	if status != FeynmanGapStatusOpen && status != FeynmanGapStatusResolved {
		return nil, fmt.Errorf("%w: status 只能是 open 或 resolved", ErrCoachQueryInput)
	}
	limit := CoachDefaultGapLimit
	if strings.TrimSpace(limitValue) != "" {
		parsed, err := strconv.Atoi(limitValue)
		if err != nil || parsed < 1 || parsed > CoachMaxGapLimit {
			return nil, fmt.Errorf("%w: limit 必须在 1-%d 之间", ErrCoachQueryInput, CoachMaxGapLimit)
		}
		limit = parsed
	}
	gaps, err := s.repo.ListGaps(ctx, userID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("查询教练薄弱点失败: %w", err)
	}
	return gaps, nil
}

func parseCoachLocalDate(value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return localDate(time.Now().In(location)), nil
	}
	return parseRequiredCoachDate("date", value, location)
}

func parseRequiredCoachDate(name, value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("%w: 缺少 %s", ErrCoachQueryInput, name)
	}
	date, err := time.ParseInLocation(time.DateOnly, value, location)
	if err != nil || date.Format(time.DateOnly) != value {
		return time.Time{}, fmt.Errorf("%w: %s 必须是 YYYY-MM-DD", ErrCoachQueryInput, name)
	}
	return date, nil
}

func localDate(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}

func daysBetween(from, to time.Time) int {
	fromYear, fromMonth, fromDay := from.Date()
	toYear, toMonth, toDay := to.Date()
	fromUTC := time.Date(fromYear, fromMonth, fromDay, 0, 0, 0, 0, time.UTC)
	toUTC := time.Date(toYear, toMonth, toDay, 0, 0, 0, 0, time.UTC)
	return int(toUTC.Sub(fromUTC) / (24 * time.Hour))
}
