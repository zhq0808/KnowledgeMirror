import { APIError } from "./knowledge";

const basePath = "/api/v1";

interface APIResponse<T> {
  code: number;
  message: string;
  data?: T;
}

async function request<T>(path: string, init: RequestInit, fallback: string): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${basePath}${path}`, { credentials: "include", ...init });
  } catch {
    throw new APIError("网络异常，请检查连接后重试", 0, -1);
  }
  let body: APIResponse<T> | null = null;
  try {
    body = (await response.json()) as APIResponse<T>;
  } catch {
    body = null;
  }
  if (!response.ok || !body || body.code !== 0) {
    throw new APIError(body?.message || fallback, response.status, body?.code ?? -1);
  }
  return body.data as T;
}

// ---------------------------------------------------------------------------
// 对话式费曼学习：练习是聊天里的一种会话状态，没有任何写接口。
// 开始/暂停/跳过/结束全部通过 /chat/stream 的自然语言完成，这里只读状态用于恢复状态条。
// ---------------------------------------------------------------------------

export type FeynmanPracticeStateName =
  | "idle"
  | "awaiting_topic"
  | "awaiting_answer"
  | "analyzing_answer"
  | "awaiting_follow_up"
  | "awaiting_retry"
  | "queue_paused";

export interface FeynmanPracticeState {
  state: FeynmanPracticeStateName;
  question: string;
  round_no: number;
  coach_task_id: string;
  original_question: string;
  retry_required: boolean;
}

export function getFeynmanPracticeState(sessionID: string): Promise<FeynmanPracticeState> {
  return request(
    `/feynman/practice-state?session_id=${encodeURIComponent(sessionID)}`,
    { method: "GET" },
    "查询练习状态失败",
  );
}
