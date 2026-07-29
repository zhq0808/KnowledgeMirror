// chat.ts 封装与后端对话接口的通信。
// 后端统一响应格式：{ code, message, data, trace_id }，code === 0 表示成功。

import type { FeynmanPracticeState } from "./feynman";

interface APIResponse {
  code: number;
  message: string;
  trace_id?: string;
}

interface GuestResponse {
  user_id: string;
  created: boolean;
}

interface SessionResponse {
  session_id: string;
}

// SessionListItem 对应后端 GET /api/v1/sessions 的单项返回。
export interface SessionListItem {
  session_id: string;
  title: string;
  status: string;
  message_count: number;
  last_message_at?: string;
  created_at: string;
}

// SessionMessage 对应后端 GET /api/v1/sessions/:session_id/messages 的单项返回。
// message_id 是后端生成的稳定 UUID 业务身份（不暴露数据库内部行主键）。
export interface SessionMessage {
  message_id: string;
  role: string;
  content: string;
  seq: number;
  retrieval?: RetrievalSources;
  created_at: string;
}

export interface RetrievalSource {
  ref: string;
  source_chunk_id: string;
  document_id: string;
  document_title: string;
  version_no: number;
  heading_path: string[];
  origin_label: string;
  trust_label: string;
  truncated: boolean;
}

export interface RetrievalSources {
  request_id?: string;
  status: string;
  candidate_count: number;
  sources: RetrievalSource[];
  quarantined_count: number;
}

const sessionIDKey = "interview_agent_session_id_v1";

// ModelOption 是可选的对话模型。目前后端使用配置里的单一模型，模型选择仅前端展示与本地记忆；
// TODO(后端): 支持 per-request model 后，把 selectedModel 随 chat 请求下发并按归属校验。
export interface ModelOption {
  id: string;
  name: string;
  desc: string;
}

export const MODELS: ModelOption[] = [
  { id: "deepseek-chat", name: "标准", desc: "日常对话，响应更快" },
  { id: "deepseek-reasoner", name: "深度思考", desc: "复杂问题，推理更强" },
];

export const DEFAULT_MODEL_ID = "deepseek-chat";
const modelIDKey = "interview_agent_model_id";

// getSelectedModelID 读取本地记住的模型；非法或缺省时回退默认模型。
export function getSelectedModelID(): string {
  const stored = localStorage.getItem(modelIDKey);
  if (stored && MODELS.some((m) => m.id === stored)) return stored;
  return DEFAULT_MODEL_ID;
}

// rememberModelID 记住当前选择的模型。
export function rememberModelID(id: string): void {
  localStorage.setItem(modelIDKey, id);
}

// createOrResumeGuest 始终请求后端：有效 HttpOnly Cookie 会恢复原 Guest，
// 没有有效凭证时才原子创建新用户。user_id 仅用于响应诊断，不在前端持久化或授权。
export async function createOrResumeGuest(): Promise<GuestResponse> {
  const res = await fetch("/api/v1/guest", {
    method: "POST",
    credentials: "include",
  });
  const body = (await res.json()) as APIResponse & { data?: GuestResponse };
  if (!res.ok || body.code !== 0 || !body.data?.user_id) {
    throw new Error(body.message || "创建试用用户失败");
  }

  if (body.data.created) {
    localStorage.removeItem(sessionIDKey);
  }
  return body.data;
}

async function requestNewSession(): Promise<string> {
  const res = await fetch("/api/v1/sessions", {
    method: "POST",
    credentials: "include",
  });
  const body = (await res.json()) as APIResponse & { data?: SessionResponse };
  if (!res.ok || body.code !== 0 || !body.data?.session_id) {
    throw new Error(body.message || "创建会话失败");
  }
  localStorage.setItem(sessionIDKey, body.data.session_id);
  return body.data.session_id;
}

export async function ensureSessionID(): Promise<string> {
  const existing = localStorage.getItem(sessionIDKey);
  if (existing) return existing;
  return requestNewSession();
}

export async function createNewSession(): Promise<string> {
  return requestNewSession();
}

// getActiveSessionID 读取当前记住的会话 ID；没有则返回 null。
export function getActiveSessionID(): string | null {
  return localStorage.getItem(sessionIDKey);
}

// rememberSessionID 记住当前活跃会话 ID，供刷新后恢复。
export function rememberSessionID(sessionID: string): void {
  localStorage.setItem(sessionIDKey, sessionID);
}

// listSessions 拉取当前访客名下的会话列表（后端按最近活跃倒序返回）。
export async function listSessions(): Promise<SessionListItem[]> {
  const res = await fetch("/api/v1/sessions", {
    method: "GET",
    credentials: "include",
  });
  const body = (await res.json()) as APIResponse & { data?: SessionListItem[] };
  if (!res.ok || body.code !== 0) {
    throw new Error(body.message || "查询会话列表失败");
  }
  return body.data ?? [];
}

// listSessionMessages 拉取指定会话内已完成、未删除的历史消息。
export async function listSessionMessages(
  sessionID: string
): Promise<SessionMessage[]> {
  const res = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionID)}/messages`,
    {
      method: "GET",
      credentials: "include",
    }
  );
  const body = (await res.json()) as APIResponse & { data?: SessionMessage[] };
  if (!res.ok || body.code !== 0) {
    throw new Error(body.message || "查询会话消息失败");
  }
  return body.data ?? [];
}

// 一帧 SSE 解析后的结果。event 为 "message"(默认增量) | "done" | "error"。
interface SSEFrame {
  event: string;
  delta?: string;
  message?: string;
  sources?: RetrievalSources;
  practice?: FeynmanPracticeState;
}

// parseFrame 解析单个 SSE 帧（形如 "event: xxx\ndata: {json}"）。
function parseFrame(frame: string): SSEFrame {
  let event = "message";
  let data = "";
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  try {
    const obj = JSON.parse(data || "{}");
    return {
      event,
      delta: obj.delta,
      message: obj.message,
      sources: event === "sources" ? (obj as RetrievalSources) : undefined,
      // 费曼练习状态挂在 done 帧上：这一轮对话可能刚刚改变了练习状态，
      // 只有跟着收尾帧一起回来，状态条才不会滞后于刚上屏的回复。
      practice: event === "done" ? (obj.feynman as FeynmanPracticeState | undefined) : undefined,
    };
  } catch {
    return { event };
  }
}

// sendChatStream 以 SSE 流式请求 /api/v1/chat/stream，每收到一段增量就回调 onDelta。
// 历史上下文由后端存储层管理，前端只提交本条消息。
// 收到 error 帧时抛异常，done 帧或流结束时正常返回。
export async function sendChatStream(
  sessionID: string,
  clientMessageID: string,
  message: string,
  onDelta: (delta: string) => void,
  onSources: (sources: RetrievalSources) => void,
  signal?: AbortSignal,
  onPracticeState?: (state: FeynmanPracticeState) => void,
  // voiceCaptureID 可选：这条消息由哪段录音转写而来。
  // 只用于把原始转写和用户实际发出的文本关联起来，不影响任何对话逻辑。
  voiceCaptureID?: string
): Promise<void> {
  const res = await fetch("/api/v1/chat/stream", {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionID,
      client_message_id: clientMessageID,
      message,
      ...(voiceCaptureID ? { voice_capture_id: voiceCaptureID } : {}),
    }),
    signal,
  });

  if (!res.ok || !res.body) {
    throw new Error("对话失败");
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true });

    // SSE 各帧以空行(\n\n)分隔，攒够一帧就取出解析，剩余半帧留在 buffer 里等下一块。
    let idx: number;
    while ((idx = buffer.indexOf("\n\n")) !== -1) {
      const frame = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 2);

      const { event, delta, message: errMsg, sources, practice } = parseFrame(frame);
      if (event === "error") throw new Error(errMsg || "对话失败");
      if (event === "done") {
        if (practice) onPracticeState?.(practice);
        return;
      }
      if (event === "sources" && sources) {
        onSources(sources);
        continue;
      }
      if (delta) onDelta(delta);
    }
  }
}
