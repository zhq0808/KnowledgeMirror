package service

import (
	"errors"
	"testing"
	"time"
)

func validCoachCommitForTest() CommitCoachAnalysisParams {
	return CommitCoachAnalysisParams{
		Attempt: CoachAttempt{
			CoachAttemptID:       "attempt-1",
			CoachTaskID:          "task-1",
			UserID:               "user-1",
			SessionID:            "session-1",
			AnswerMessageID:      "message-1",
			OriginalQuestionText: "为什么要使用 Outbox？",
			AnalysisJSON:         []byte(`{"summary":"缺少失败补偿"}`),
			Outcome:              CoachAttemptOutcomeRetryRequired,
			PromptVersion:        "coach-v1",
			ModelName:            "test-model",
		},
		Gaps: []CoachGapEvidence{{
			AttemptGapID:        "attempt-gap-1",
			GapID:               "gap-1",
			GapKey:              "  Outbox  Compensation ",
			GapType:             CoachGapTypeKnowledge,
			DiagnosticDimension: FeynmanDimensionCausalChain,
			Classification:      CoachGapClassificationNew,
			Title:               "缺少补偿链路",
			Severity:            4,
			IsFocus:             true,
			EvidenceJSON:        []byte(`{"dimension":"causal_chain"}`),
		}},
		CorrectionDate: time.Date(2026, 8, 7, 0, 0, 0, 0, time.Local),
		PracticeState: FeynmanPracticeState{
			SessionID:            "session-1",
			UserID:               "user-1",
			State:                FeynmanStateAwaitingRetry,
			ActiveQuestionText:   "为什么要使用 Outbox？",
			QuestionOrigin:       FeynmanQuestionOriginCoachTask,
			CoachTaskID:          "task-1",
			OriginalQuestionText: "为什么要使用 Outbox？",
			RetryRequired:        true,
			RoundNo:              1,
		},
	}
}

func TestFixedCoachReviewDatesUseCalendarOffsets(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	anchor := time.Date(2026, 8, 7, 18, 30, 0, 0, location)
	got := FixedCoachReviewDates(anchor)
	want := [3]string{"2026-08-08", "2026-08-10", "2026-08-14"}
	for index := range got {
		if got[index].Format(time.DateOnly) != want[index] || got[index].Hour() != 0 {
			t.Fatalf("stage %d date = %v, want %s local midnight", index+1, got[index], want[index])
		}
	}
}

func TestDecideCoachReviewLifecycle(t *testing.T) {
	task := CoachDailyTask{SourceGapID: "target-gap", SourceReviewID: "review-2"}
	passed := DecideCoachReviewLifecycle(task, []CoachGapEvidence{{ForceCanonicalGapID: "different-gap"}})
	if !passed.IsRetest || passed.TargetRecurred || passed.CurrentReviewStatus != FeynmanGapReviewStatusPassed {
		t.Fatalf("target absent decision = %+v", passed)
	}
	failed := DecideCoachReviewLifecycle(task, []CoachGapEvidence{{ForceCanonicalGapID: "target-gap"}})
	if !failed.TargetRecurred || failed.CurrentReviewStatus != FeynmanGapReviewStatusFailed {
		t.Fatalf("target recurrent decision = %+v", failed)
	}
	initial := DecideCoachReviewLifecycle(CoachDailyTask{}, nil)
	if initial.IsRetest || initial.CurrentReviewStatus != "" {
		t.Fatalf("initial decision = %+v", initial)
	}
}

func TestNormalizeCoachGapKey(t *testing.T) {
	got, err := NormalizeCoachGapKey("  Outbox   Compensation  ")
	if err != nil {
		t.Fatalf("NormalizeCoachGapKey() error = %v", err)
	}
	if got != "outbox-compensation" {
		t.Fatalf("NormalizeCoachGapKey() = %q", got)
	}
	if _, err := NormalizeCoachGapKey("   "); !errors.Is(err, ErrCoachAnalysisInput) {
		t.Fatalf("empty key error = %v, want ErrCoachAnalysisInput", err)
	}
}

func TestClassifyCoachGap(t *testing.T) {
	prior := map[string]PriorGapEvidence{
		"outbox-compensation": {GapID: "gap-existing", GapKey: "outbox-compensation"},
	}
	classification, gapID, err := ClassifyCoachGap(" Outbox Compensation ", prior)
	if err != nil || classification != CoachGapClassificationRecurrent || gapID != "gap-existing" {
		t.Fatalf("existing classification = (%q, %q, %v)", classification, gapID, err)
	}
	classification, gapID, err = ClassifyCoachGap("new-gap", prior)
	if err != nil || classification != CoachGapClassificationNew || gapID != "" {
		t.Fatalf("new classification = (%q, %q, %v)", classification, gapID, err)
	}
}

func TestValidateCoachAnalysisCommitNormalizesGapKey(t *testing.T) {
	params := validCoachCommitForTest()
	if err := ValidateCoachAnalysisCommit(&params); err != nil {
		t.Fatalf("ValidateCoachAnalysisCommit() error = %v", err)
	}
	if params.Gaps[0].GapKey != "outbox-compensation" {
		t.Fatalf("gap key = %q", params.Gaps[0].GapKey)
	}
}

func TestValidateCoachAnalysisCommitRejectsPassedRecurrentTarget(t *testing.T) {
	params := validCoachCommitForTest()
	params.Attempt.Outcome = CoachAttemptOutcomePassed
	params.Gaps[0].ForceCanonicalGapID = "target-gap"
	params.Gaps[0].RequiresCorrection = true
	params.Gaps[0].IsFocus = false
	params.ReviewDecision = CoachReviewDecision{IsRetest: true, TargetRecurred: true, CurrentReviewStatus: FeynmanGapReviewStatusFailed}
	params.PracticeState = FeynmanPracticeState{UserID: "user-1", SessionID: "session-1", State: FeynmanStateIdle}
	if err := ValidateCoachAnalysisCommit(&params); !errors.Is(err, ErrCoachAnalysisInput) {
		t.Fatalf("error = %v, want ErrCoachAnalysisInput", err)
	}
}

func TestValidateCoachAnalysisCommitRejectsInvalidClassificationInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CommitCoachAnalysisParams)
	}{
		{name: "分析必须是对象", mutate: func(p *CommitCoachAnalysisParams) { p.Attempt.AnalysisJSON = []byte(`[]`) }},
		{name: "分析不能复制回答", mutate: func(p *CommitCoachAnalysisParams) { p.Attempt.AnalysisJSON = []byte(`{"answer_text":"完整回答"}`) }},
		{name: "重复键", mutate: func(p *CommitCoachAnalysisParams) {
			p.Gaps = append(p.Gaps, p.Gaps[0])
			p.Gaps[1].AttemptGapID = "attempt-gap-2"
			p.Gaps[1].GapID = "gap-2"
		}},
		{name: "重答必须有 focus", mutate: func(p *CommitCoachAnalysisParams) { p.Gaps[0].IsFocus = false }},
		{name: "严重度越界", mutate: func(p *CommitCoachAnalysisParams) { p.Gaps[0].Severity = 6 }},
		{name: "重答状态不一致", mutate: func(p *CommitCoachAnalysisParams) { p.PracticeState.State = FeynmanStateAwaitingAnswer }},
		{name: "缺少回答本地日期", mutate: func(p *CommitCoachAnalysisParams) { p.CorrectionDate = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := validCoachCommitForTest()
			tt.mutate(&params)
			if err := ValidateCoachAnalysisCommit(&params); !errors.Is(err, ErrCoachAnalysisInput) {
				t.Fatalf("error = %v, want ErrCoachAnalysisInput", err)
			}
		})
	}
}
