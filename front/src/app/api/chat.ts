// chat.ts 封装与后端对话接口的通信。
// 后端统一响应格式：{ code, message, data, trace_id }，code === 0 表示成功。

import type { FeynmanPracticeState } from "./feynman";

interface APIResponse {
  code: number;
  message: string;
  trace_id?: string;
  retryable?: boolean;
}

interface GuestResponse {
  user_id: string;
  created: boolean;
}

interface SessionResponse {
  session_id: string;
}

export interface SessionListItem {
  session_id: string;
  title: string;
  status: string;
  message_count: number;
  last_message_at?: string;
  created_at: string;
}

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

export async function createOrResumeGuest(): Promise<GuestResponse> {
  const res = await fetch("/api/v1/guest", {
    method: "POST",
    credentials: "include",
  });
  const body = (await res.json()) as APIResponse & { data?: GuestResponse };
  if (!res.ok || body.code !== 0 || !body.data?.user_id) {
    throw new Error(body.message || "创建试用用户失败");
  }

  if (body.data.created) localStorage.removeItem(sessionIDKey);
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

export function getActiveSessionID(): string | null {
  return localStorage.getItem(sessionIDKey);
}

export function rememberSessionID(sessionID: string): void {
  localStorage.setItem(sessionIDKey, sessionID);
}

export async function listSessions(): Promise<SessionListItem[]> {
  const res = await fetch("/api/v1/sessions", {
    method: "GET",
    credentials: "include",
  });
  const body = (await res.json()) as APIResponse & { data?: SessionListItem[] };
  if (!res.ok || body.code !== 0) throw new Error(body.message || "查询会话列表失败");
  return body.data ?? [];
}

export async function listSessionMessages(sessionID: string): Promise<SessionMessage[]> {
  const res = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionID)}/messages`,
    { method: "GET", credentials: "include" },
  );
  const body = (await res.json()) as APIResponse & { data?: SessionMessage[] };
  if (!res.ok || body.code !== 0) throw new Error(body.message || "查询会话消息失败");
  return body.data ?? [];
}

export class ChatStreamError extends Error {
  readonly code: number;
  readonly retryable: boolean;

  constructor(message: string, code = -1, retryable = true) {
    super(message);
    this.name = "ChatStreamError";
    this.code = code;
    this.retryable = retryable;
  }
}

export interface ChatSSEFrame {
  event: string;
  code?: number;
  delta?: string;
  message?: string;
  retryable?: boolean;
  sources?: RetrievalSources;
  practice?: FeynmanPracticeState;
}

export function parseChatSSEFrame(frame: string): ChatSSEFrame {
  let event = "message";
  let data = "";
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  try {
    const obj = JSON.parse(data || "{}") as Record<string, unknown>;
    return {
      event,
      code: typeof obj.code === "number" ? obj.code : undefined,
      delta: typeof obj.delta === "string" ? obj.delta : undefined,
      message: typeof obj.message === "string" ? obj.message : undefined,
      retryable: typeof obj.retryable === "boolean" ? obj.retryable : undefined,
      sources: event === "sources" ? (obj as unknown as RetrievalSources) : undefined,
      practice:
        event === "done"
          ? (obj.feynman as FeynmanPracticeState | undefined)
          : undefined,
    };
  } catch {
    return { event };
  }
}

export interface ConsumeChatStreamCallbacks {
  onDelta: (delta: string) => void;
  onSources: (sources: RetrievalSources) => void;
  onPracticeState?: (state: FeynmanPracticeState) => void;
}

export async function consumeChatSSEStream(
  stream: ReadableStream<Uint8Array>,
  callbacks: ConsumeChatStreamCallbacks,
): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) {
      buffer += decoder.decode();
      throw new ChatStreamError("对话流意外中断，请重试", -1, true);
    }
    buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, "\n");

    let index: number;
    while ((index = buffer.indexOf("\n\n")) !== -1) {
      const frame = parseChatSSEFrame(buffer.slice(0, index));
      buffer = buffer.slice(index + 2);
      if (frame.event === "error") {
        throw new ChatStreamError(
          frame.message || "对话失败",
          frame.code ?? -1,
          frame.retryable ?? true,
        );
      }
      if (frame.event === "done") {
        if (frame.practice) callbacks.onPracticeState?.(frame.practice);
        return;
      }
      if (frame.event === "sources" && frame.sources) {
        callbacks.onSources(frame.sources);
      } else if (frame.delta) {
        callbacks.onDelta(frame.delta);
      }
    }
  }
}

export interface SendChatStreamOptions {
  signal?: AbortSignal;
  onPracticeState?: (state: FeynmanPracticeState) => void;
  voiceCaptureID?: string;
  coachTaskID?: string;
  localDate?: string;
}

export async function sendChatStream(
  sessionID: string,
  clientMessageID: string,
  message: string,
  onDelta: (delta: string) => void,
  onSources: (sources: RetrievalSources) => void,
  options: SendChatStreamOptions = {},
): Promise<void> {
  const { signal, onPracticeState, voiceCaptureID, coachTaskID, localDate } = options;
  const res = await fetch("/api/v1/chat/stream", {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionID,
      client_message_id: clientMessageID,
      message,
      ...(voiceCaptureID ? { voice_capture_id: voiceCaptureID } : {}),
      ...(coachTaskID ? { coach_task_id: coachTaskID } : {}),
      ...(localDate ? { local_date: localDate } : {}),
    }),
    signal,
  });

  if (!res.ok || !res.body) {
    let body: APIResponse | null = null;
    try {
      body = (await res.json()) as APIResponse;
    } catch {
      body = null;
    }
    throw new ChatStreamError(
      body?.message || "对话失败",
      body?.code ?? res.status,
      body?.retryable ?? res.status >= 500,
    );
  }

  await consumeChatSSEStream(res.body, { onDelta, onSources, onPracticeState });
}
