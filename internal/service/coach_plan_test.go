package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCoachPlanRepository struct {
	plan        CoachDailyPlan
	progress    CoachProgress
	gaps        []FeynmanGap
	ensureCalls int
	gotUserID   string
	gotDate     time.Time
	gotStatus   string
	gotLimit    int
	ensureErr   error
	progressErr error
	gapsErr     error
}

func (r *fakeCoachPlanRepository) EnsureDailyPlan(_ context.Context, userID string, date time.Time) (CoachDailyPlan, error) {
	r.ensureCalls++
	r.gotUserID = userID
	r.gotDate = date
	return r.plan, r.ensureErr
}
func (r *fakeCoachPlanRepository) GetProgress(_ context.Context, _ string, from, to time.Time) (CoachProgress, error) {
	result := r.progress
	result.From = from
	result.To = to
	return result, r.progressErr
}
func (r *fakeCoachPlanRepository) ListGaps(_ context.Context, _ string, status string, limit int) ([]FeynmanGap, error) {
	r.gotStatus, r.gotLimit = status, limit
	return r.gaps, r.gapsErr
}

func fixedCoachNow() time.Time {
	return time.Date(2026, 8, 7, 10, 30, 0, 0, time.Local)
}

func TestCoachServiceTodayEnsuresPlanAndReturnsActionableEmptyState(t *testing.T) {
	repo := &fakeCoachPlanRepository{plan: CoachDailyPlan{Date: localDate(fixedCoachNow())}}
	svc := NewCoachService(repo, fixedCoachNow)
	plan, err := svc.Today(context.Background(), "user-1", "2026-08-07")
	if err != nil {
		t.Fatalf("Today() error = %v", err)
	}
	if repo.ensureCalls != 1 || repo.gotDate.Format(time.DateOnly) != "2026-08-07" {
		t.Fatalf("EnsureDailyPlan calls/date = %d/%s", repo.ensureCalls, repo.gotDate)
	}
	if plan.EmptyState == nil || plan.EmptyState.ActionPath == "" || plan.Required != nil || len(plan.Optional) != 0 {
		t.Fatalf("empty plan = %+v", plan)
	}
}

func TestCoachServiceTodayPreservesActiveTask(t *testing.T) {
	active := CoachDailyTask{CoachTaskID: "task-1", Status: CoachTaskStatusInProgress}
	repo := &fakeCoachPlanRepository{plan: CoachDailyPlan{Date: localDate(fixedCoachNow()), ActiveTask: &active}}
	plan, err := NewCoachService(repo, fixedCoachNow).Today(context.Background(), "user-1", "")
	if err != nil || plan.ActiveTask == nil || plan.EmptyState != nil {
		t.Fatalf("active plan = %+v, err=%v", plan, err)
	}
}

func TestCoachServiceTodayValidatesLocalDateWindow(t *testing.T) {
	svc := NewCoachService(&fakeCoachPlanRepository{}, fixedCoachNow)
	for _, date := range []string{"bad", "2026-08-05", "2026-08-09"} {
		if _, err := svc.Today(context.Background(), "user-1", date); !errors.Is(err, ErrCoachQueryInput) {
			t.Fatalf("Today(%q) error = %v", date, err)
		}
	}
}

func TestDaysBetweenUsesCalendarDatesAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone data unavailable: %v", err)
	}
	beforeSpring := time.Date(2026, 3, 7, 0, 0, 0, 0, location)
	afterSpring := time.Date(2026, 3, 9, 0, 0, 0, 0, location)
	if got := daysBetween(beforeSpring, afterSpring); got != 2 {
		t.Fatalf("spring DST days = %d, want 2", got)
	}
	beforeFall := time.Date(2026, 10, 31, 0, 0, 0, 0, location)
	afterFall := time.Date(2026, 11, 2, 0, 0, 0, 0, location)
	if got := daysBetween(beforeFall, afterFall); got != 2 {
		t.Fatalf("fall DST days = %d, want 2", got)
	}
}

func TestCoachServiceProgressValidatesNinetyDayRange(t *testing.T) {
	svc := NewCoachService(&fakeCoachPlanRepository{}, fixedCoachNow)
	if _, err := svc.Progress(context.Background(), "user-1", "2026-05-10", "2026-08-07"); err != nil {
		t.Fatalf("90-day Progress() error = %v", err)
	}
	if _, err := svc.Progress(context.Background(), "user-1", "2026-05-09", "2026-08-07"); !errors.Is(err, ErrCoachQueryInput) {
		t.Fatalf("91-day Progress() error = %v", err)
	}
	if _, err := svc.Progress(context.Background(), "user-1", "2026-08-08", "2026-08-07"); !errors.Is(err, ErrCoachQueryInput) {
		t.Fatalf("reversed Progress() error = %v", err)
	}
}

func TestCoachServiceGapsDefaultsAndValidatesFilters(t *testing.T) {
	repo := &fakeCoachPlanRepository{}
	svc := NewCoachService(repo, fixedCoachNow)
	if _, err := svc.Gaps(context.Background(), "user-1", "", ""); err != nil {
		t.Fatalf("Gaps() error = %v", err)
	}
	if repo.gotStatus != FeynmanGapStatusOpen || repo.gotLimit != CoachDefaultGapLimit {
		t.Fatalf("defaults = %q/%d", repo.gotStatus, repo.gotLimit)
	}
	for _, input := range [][2]string{{"unknown", "50"}, {"open", "0"}, {"open", "101"}, {"open", "x"}} {
		if _, err := svc.Gaps(context.Background(), "user-1", input[0], input[1]); !errors.Is(err, ErrCoachQueryInput) {
			t.Fatalf("Gaps(%q,%q) error = %v", input[0], input[1], err)
		}
	}
}
