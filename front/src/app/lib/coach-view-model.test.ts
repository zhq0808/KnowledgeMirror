/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import type { CoachGap, CoachProgress, CoachToday } from "../api/coach.ts";
import {
  buildCoachStatusTags,
  calendarProgress,
  coachTaskCTA,
  groupGaps,
  localDateKey,
  requiredCompletionStreak,
} from "./coach-view-model.ts";

function progress(days: CoachProgress["days"]): CoachProgress {
  return {
    from: "2026-08-01",
    to: "2026-08-07",
    required_total: days.reduce((sum, day) => sum + day.required_total, 0),
    required_completed: days.reduce((sum, day) => sum + day.required_completed, 0),
    optional_total: days.reduce((sum, day) => sum + day.optional_total, 0),
    optional_completed: days.reduce((sum, day) => sum + day.optional_completed, 0),
    pending: days.reduce((sum, day) => sum + day.pending, 0),
    in_progress: days.reduce((sum, day) => sum + day.in_progress, 0),
    awaiting_retry: 0,
    completed: days.reduce((sum, day) => sum + day.completed, 0),
    skipped: days.reduce((sum, day) => sum + day.skipped, 0),
    days,
  };
}

function day(date: string, requiredCompleted: number, optionalCompleted = 0) {
  return {
    date,
    required_total: 1,
    required_completed: requiredCompleted,
    optional_total: 1,
    optional_completed: optionalCompleted,
    pending: 0,
    in_progress: 0,
    completed: requiredCompleted + optionalCompleted,
    skipped: 0,
  };
}

test("formats local calendar dates without UTC conversion", () => {
  assert.equal(localDateKey(new Date(2026, 7, 7, 23, 59)), "2026-08-07");
});

test("maps every server task status to an explicit CTA", () => {
  assert.deepEqual(coachTaskCTA("pending"), { label: "开始", disabled: false });
  assert.deepEqual(coachTaskCTA("in_progress"), { label: "继续", disabled: false });
  assert.deepEqual(coachTaskCTA("awaiting_retry"), { label: "需要重答", disabled: false });
  assert.equal(coachTaskCTA("completed").disabled, true);
  assert.equal(coachTaskCTA("skipped").label, "已跳过");
});

test("builds real calendar totals and completed counts", () => {
  assert.deepEqual(calendarProgress(progress([day("2026-08-07", 1, 1)])), {
    "2026-08-07": { completed: 2, total: 2 },
  });
});

test("streak ends today or the latest prescribed day and breaks on missing dates", () => {
  const current = progress([
    day("2026-08-04", 1),
    day("2026-08-05", 1),
    day("2026-08-06", 1),
  ]);
  assert.equal(requiredCompletionStreak(current, new Date(2026, 7, 7)), 3);

  const broken = progress([day("2026-08-04", 1), day("2026-08-06", 1)]);
  assert.equal(requiredCompletionStreak(broken, new Date(2026, 7, 7)), 1);
  assert.equal(
    requiredCompletionStreak(progress([day("2026-08-07", 0)]), new Date(2026, 7, 7)),
    0,
  );
});

test("status tag uses only progress totals and daily completed counts", () => {
  const value = progress([day("2026-08-06", 1), day("2026-08-07", 1, 1)]);
  const today = { date: "2026-08-07", required: null, optional: [] } satisfies CoachToday;
  const tags = buildCoachStatusTags(value, today, new Date(2026, 7, 7));
  assert.equal(tags[0].label, "必做连续 2 天");
  assert.deepEqual(tags[0].sparklineData, [{ v: 1 }, { v: 2 }]);
  assert.match(tags[0].summary, /必做完成 2\/2，选做完成 1\/2/);
});

test("groups open gaps into the four backend diagnostic types", () => {
  const base = {
    gap_id: "gap",
    gap_key: "key",
    title: "title",
    description: "description",
    status: "open",
    evidence_count: 1,
    first_seen_at: "2026-08-01T00:00:00Z",
    last_seen_at: "2026-08-07T00:00:00Z",
  } as const;
  const gaps: CoachGap[] = [
    { ...base, gap_id: "1", gap_type: "knowledge_gap" },
    { ...base, gap_id: "2", gap_type: "recall_failure" },
    { ...base, gap_id: "3", gap_type: "expression_structure" },
    { ...base, gap_id: "4", gap_type: "missing_project_evidence" },
  ];
  const grouped = groupGaps(gaps);
  assert.deepEqual(Object.values(grouped).map((items) => items.length), [1, 1, 1, 1]);
});
