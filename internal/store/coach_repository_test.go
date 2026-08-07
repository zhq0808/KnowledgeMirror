package store

import (
	"testing"
	"time"

	"KnowledgeMirror/internal/service"
)

func TestBuildCoachDailyPlanSeparatesTerminalTasks(t *testing.T) {
	date := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	tasks := []service.CoachDailyTask{
		{CoachTaskID: "skipped-required", PlanRole: service.CoachPlanRoleRequired, Status: service.CoachTaskStatusSkipped},
		{CoachTaskID: "completed-optional", PlanRole: service.CoachPlanRoleOptional, Status: service.CoachTaskStatusCompleted},
		{CoachTaskID: "pending-required", PlanRole: service.CoachPlanRoleRequired, Status: service.CoachTaskStatusPending},
		{CoachTaskID: "pending-optional", PlanRole: service.CoachPlanRoleOptional, Status: service.CoachTaskStatusPending},
	}
	plan := buildCoachDailyPlan(date, tasks)
	if plan.Required == nil || plan.Required.CoachTaskID != "pending-required" {
		t.Fatalf("required = %+v", plan.Required)
	}
	if len(plan.Optional) != 1 || plan.Optional[0].CoachTaskID != "pending-optional" {
		t.Fatalf("optional = %+v", plan.Optional)
	}
	if len(plan.TerminalTasks) != 2 || plan.TerminalTasks[0].CoachTaskID != "skipped-required" || plan.TerminalTasks[1].CoachTaskID != "completed-optional" {
		t.Fatalf("terminal tasks = %+v", plan.TerminalTasks)
	}
}
