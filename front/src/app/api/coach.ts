import { APIError } from "./knowledge";
import { localDateKey } from "../lib/coach-view-model";

const basePath = "/api/v1";

interface APIResponse<T> {
  code: number;
  message: string;
  data?: T;
}

export type CoachTaskStatus =
  | "pending"
  | "in_progress"
  | "awaiting_retry"
  | "completed"
  | "skipped";
export type CoachPlanRole = "required" | "optional";
export type CoachTaskType = "feynman_new" | "feynman_retry";
export type CoachGapStatus = "open" | "resolved";
export type CoachGapType =
  | "knowledge_gap"
  | "recall_failure"
  | "expression_structure"
  | "missing_project_evidence";

export interface CoachTask {
  coach_task_id: string;
  date: string;
  task_type: CoachTaskType;
  plan_role: CoachPlanRole;
  status: CoachTaskStatus;
  question: string;
  knowledge_point_id?: string;
  source_gap_id?: string;
  session_id?: string;
  carried_over: boolean;
  started_at?: string;
  completed_at?: string;
}

export interface CoachEmptyState {
  code: string;
  message: string;
  action: string;
  action_path: string;
}

export interface CoachToday {
  date: string;
  required: CoachTask | null;
  optional: CoachTask[];
  active_task?: CoachTask;
  terminal_tasks: CoachTask[];
  empty_state?: CoachEmptyState;
}

export interface CoachProgressDay {
  date: string;
  required_total: number;
  required_completed: number;
  optional_total: number;
  optional_completed: number;
  pending: number;
  in_progress: number;
  awaiting_retry: number;
  completed: number;
  skipped: number;
}

export interface CoachProgress {
  from: string;
  to: string;
  required_total: number;
  required_completed: number;
  optional_total: number;
  optional_completed: number;
  pending: number;
  in_progress: number;
  awaiting_retry: number;
  completed: number;
  skipped: number;
  days: CoachProgressDay[];
}

export interface CoachGap {
  gap_id: string;
  knowledge_point_id?: string;
  gap_key: string;
  gap_type: CoachGapType;
  title: string;
  description: string;
  status: CoachGapStatus;
  evidence_count: number;
  first_seen_at: string;
  last_seen_at: string;
  next_review_date?: string;
  next_review_at?: string;
}

async function getJSON<T>(path: string, fallback: string): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${basePath}${path}`, {
      method: "GET",
      credentials: "include",
    });
  } catch {
    throw new APIError("网络异常，请检查连接后重试", 0, -1);
  }

  let body: APIResponse<T> | null = null;
  try {
    body = (await response.json()) as APIResponse<T>;
  } catch {
    body = null;
  }
  if (!response.ok || !body || body.code !== 0 || body.data === undefined) {
    throw new APIError(body?.message || fallback, response.status, body?.code ?? -1);
  }
  return body.data;
}

export function getCoachToday(date = new Date()): Promise<CoachToday> {
  return getJSON(`/coach/today?date=${encodeURIComponent(localDateKey(date))}`, "查询今日教练计划失败");
}

export function getCoachProgress(from: Date, to: Date): Promise<CoachProgress> {
  const query = new URLSearchParams({ from: localDateKey(from), to: localDateKey(to) });
  return getJSON(`/coach/progress?${query.toString()}`, "查询教练进度失败");
}

export function getCoachGaps(
  status: CoachGapStatus = "open",
  limit = 50,
): Promise<CoachGap[]> {
  const query = new URLSearchParams({ status, limit: String(limit) });
  return getJSON(`/coach/gaps?${query.toString()}`, "查询教练薄弱点失败");
}
