package store

import (
	"os"
	"strings"
	"testing"
)

func TestCoachMigrationSQLGuardsPopulatedData(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile("../../migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	up18 := read("000018_coach_strict_retry_reviews.up.sql")
	down18 := read("000018_coach_strict_retry_reviews.down.sql")
	for name, sql := range map[string]string{"000018 up": up18, "000018 down": down18} {
		drop := strings.Index(sql, "DROP TRIGGER trg_coach_attempt_gaps_append_only")
		update := strings.Index(sql, "UPDATE coach_attempt_gaps")
		recreate := strings.Index(sql, "CREATE TRIGGER trg_coach_attempt_gaps_append_only")
		if drop < 0 || update < 0 || recreate < 0 || !(drop < update && update < recreate) {
			t.Fatalf("%s does not bracket append-only backfill safely", name)
		}
	}
	up17 := read("000017_coach_daily_plan_queries.up.sql")
	if !strings.Contains(up17, "task_type = 'feynman_retry' AND source_gap_id IS NOT NULL") {
		t.Fatal("000017 does not tolerate populated legacy retry tasks")
	}
	up19 := read("000019_coach_fixed_gap_review_lifecycle.up.sql")
	if !strings.Contains(up19, "task.task_date") || strings.Contains(up19, "scheduled_for::date - 1") {
		t.Fatal("000019 historical anchor is not derived from source task date")
	}
	down19 := read("000019_coach_fixed_gap_review_lifecycle.down.sql")
	practiceReset := strings.Index(down19, "UPDATE feynman_practice_states AS practice")
	taskSkip := strings.Index(down19, "UPDATE coach_daily_tasks AS task")
	if practiceReset < 0 || taskSkip < 0 || practiceReset > taskSkip {
		t.Fatal("000019 down does not reset practice before skipping tasks")
	}
}
