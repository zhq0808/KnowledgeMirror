import type {
  CoachGap,
  CoachGapType,
  CoachProgress,
  CoachTask,
  CoachTaskStatus,
  CoachToday,
} from "../api/coach";

export interface CoachTaskCTA {
  label: "开始" | "继续" | "需要重答" | "已完成" | "已跳过";
  disabled: boolean;
}

export interface CalendarProgress {
  completed: number;
  total: number;
}

export interface CoachStatusTag {
  id: string;
  emoji: string;
  label: string;
  color: string;
  state: "active";
  sparklineData: { v: number }[];
  summary: string;
}

export const GAP_LABELS: Record<CoachGapType, string> = {
  knowledge_gap: "知识缺口",
  recall_failure: "提取失败",
  expression_structure: "表达结构",
  missing_project_evidence: "缺少项目证据",
};

export function localDateKey(date: Date): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

export function addLocalDays(date: Date, amount: number): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() + amount);
}

export function coachTaskCTA(status: CoachTaskStatus): CoachTaskCTA {
  switch (status) {
    case "pending":
      return { label: "开始", disabled: false };
    case "in_progress":
      return { label: "继续", disabled: false };
    case "awaiting_retry":
      return { label: "需要重答", disabled: false };
    case "completed":
      return { label: "已完成", disabled: true };
    case "skipped":
      return { label: "已跳过", disabled: true };
  }
}

export function coachTodayTasks(today: CoachToday | null): CoachTask[] {
  if (!today) return [];
  const ordered = [
    today.active_task,
    today.required,
    ...today.optional,
    ...(today.terminal_tasks ?? []),
  ].filter((task): task is CoachTask => task != null);
  return ordered.filter(
    (task, index) => ordered.findIndex((item) => item.coach_task_id === task.coach_task_id) === index,
  );
}

export function calendarProgress(progress: CoachProgress | null): Record<string, CalendarProgress> {
  return Object.fromEntries(
    (progress?.days ?? []).map((day) => [
      day.date,
      {
        completed: day.required_completed + day.optional_completed,
        total: day.required_total + day.optional_total,
      },
    ]),
  );
}

/**
 * Counts consecutive local calendar days whose required task is fully completed.
 * The run ends at today when today has a required task; otherwise it ends at the
 * most recent day with a required task. An incomplete required task at that end
 * makes the current streak zero, and a missing calendar day breaks the run.
 */
export function requiredCompletionStreak(
  progress: CoachProgress | null,
  today = new Date(),
): number {
  const eligible = (progress?.days ?? [])
    .filter((day) => day.date <= localDateKey(today) && day.required_total > 0)
    .sort((left, right) => left.date.localeCompare(right.date));
  if (eligible.length === 0) return 0;

  const byDate = new Map(eligible.map((day) => [day.date, day]));
  const last = eligible[eligible.length - 1];
  if (last.required_completed < last.required_total) return 0;

  let streak = 0;
  let cursor = new Date(`${last.date}T00:00:00`);
  while (true) {
    const day = byDate.get(localDateKey(cursor));
    if (!day || day.required_completed < day.required_total) break;
    streak += 1;
    cursor = addLocalDays(cursor, -1);
  }
  return streak;
}

export function buildCoachStatusTags(
  progress: CoachProgress | null,
  today: CoachToday | null,
  now = new Date(),
): CoachStatusTag[] {
  if (!progress || !today) return [];
  const streak = requiredCompletionStreak(progress, now);
  const dailyCompleted = progress.days.map(
    (day) => day.required_completed + day.optional_completed,
  );
  return [
    {
      id: "required-streak",
      emoji: "✓",
      label: `必做连续 ${streak} 天`,
      color: "bg-[#E8F0E8] text-[#2E5E3E]",
      state: "active",
      sparklineData: dailyCompleted.map((v) => ({ v })),
      summary: `查询区间内必做完成 ${progress.required_completed}/${progress.required_total}，选做完成 ${progress.optional_completed}/${progress.optional_total}。`,
    },
  ];
}

export function groupGaps(gaps: CoachGap[]): Record<CoachGapType, CoachGap[]> {
  const grouped: Record<CoachGapType, CoachGap[]> = {
    knowledge_gap: [],
    recall_failure: [],
    expression_structure: [],
    missing_project_evidence: [],
  };
  for (const gap of gaps) grouped[gap.gap_type].push(gap);
  return grouped;
}
