import { audioFileExtension } from "../lib/audio";
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

export interface FeynmanAudioTask {
  audio_task_id: string;
  attempt_no: number;
  status: "uploaded" | "transcribing" | "transcribed" | "failed";
  mime_type: string;
  size_bytes: number;
  duration_ms?: number;
  stt_provider?: string;
  stt_model?: string;
  raw_transcript?: string;
  transcript_error?: string;
  created_at: string;
}

export interface FeynmanConfirmation {
  confirmation_id: string;
  raw_transcript: string;
  confirmed_transcript: string;
  edited: boolean;
  confirmed_at: string;
}

export interface FeynmanAttempt {
  attempt_id: string;
  knowledge_point_id: string;
  status: "open" | "transcribing" | "transcribed" | "failed" | "transcript_confirmed";
  active_audio_task?: FeynmanAudioTask;
  confirmation?: FeynmanConfirmation;
  created_at: string;
  updated_at: string;
}

export interface RubricCriterion {
  dimension: string;
  label: string;
  weight: number;
  description: string;
}

export interface FeynmanRubric {
  rubric_id: string;
  knowledge_point_id: string;
  version_no: number;
  template_version: string;
  criteria: RubricCriterion[];
  created_at: string;
}

export interface DimensionEvaluation {
  dimension: string;
  score: number;
  feedback: string;
  output_quotes: string[];
  source_refs: string[];
}

export interface EvidenceCandidate {
  claim: string;
  rationale: string;
  evidence_scope: "learning";
  output_quotes: string[];
  source_refs: string[];
}

export interface EvaluationPayload {
  summary: string;
  insufficient_sources: boolean;
  dimensions: DimensionEvaluation[];
  evidence_candidate: EvidenceCandidate;
}

export interface EvaluationSource {
  ref: string;
  source_chunk_id: string;
  document_id: string;
  version_id: string;
  document_title: string;
  version_no: number;
  heading_path: string[];
  content: string;
  trust_level: string;
}

export interface FeynmanEvaluation {
  evaluation_id: string;
  attempt_id: string;
  confirmation_id: string;
  rubric_id: string;
  knowledge_point_id: string;
  status: "evaluating" | "proposed" | "failed";
  prompt_version: string;
  model_name: string;
  confirmed_transcript: string;
  payload?: EvaluationPayload;
  sources: EvaluationSource[];
  error_message?: string;
  decision?: {
    decision_id: string;
    decision: "confirmed" | "rejected";
    final_payload?: EvaluationPayload;
    decision_note?: string;
    decided_at: string;
  };
  created_at: string;
  updated_at: string;
}

export function newFeynmanIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `feynman-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

export function createFeynmanAttempt(knowledgePointID: string, idempotencyKey: string): Promise<FeynmanAttempt> {
  return request("/feynman/attempts", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ knowledge_point_id: knowledgePointID, idempotency_key: idempotencyKey }),
  }, "创建练习失败");
}

export function getFeynmanAttempt(attemptID: string): Promise<FeynmanAttempt> {
  return request(`/feynman/attempts/${attemptID}`, { method: "GET" }, "查询练习失败");
}

export function uploadFeynmanAudio(attemptID: string, audio: Blob, durationMs: number): Promise<FeynmanAttempt> {
  const form = new FormData();
  form.append("audio", audio, `feynman.${audioFileExtension(audio.type)}`);
  form.append("duration_ms", String(Math.max(1, Math.round(durationMs))));
  return request(`/feynman/attempts/${attemptID}/audio`, { method: "POST", body: form }, "上传录音失败");
}

export function confirmFeynmanTranscript(attemptID: string, confirmedTranscript: string): Promise<FeynmanAttempt> {
  return request(`/feynman/attempts/${attemptID}/confirm`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ confirmed_transcript: confirmedTranscript }),
  }, "确认转写失败");
}

export function getFeynmanRubric(knowledgePointID: string): Promise<FeynmanRubric> {
  return request(`/knowledge-points/${knowledgePointID}/rubric`, { method: "GET" }, "查询评分标准失败");
}

export function evaluateFeynmanAttempt(attemptID: string): Promise<FeynmanEvaluation> {
  return request(`/feynman/attempts/${attemptID}/evaluate`, { method: "POST" }, "评估练习失败");
}

export function getFeynmanEvaluation(attemptID: string): Promise<FeynmanEvaluation> {
  return request(`/feynman/attempts/${attemptID}/evaluation`, { method: "GET" }, "查询评估失败");
}

export function decideFeynmanEvaluation(evaluationID: string, decision: "confirmed" | "rejected", finalPayload?: EvaluationPayload, decisionNote = ""): Promise<FeynmanEvaluation> {
  return request(`/feynman/evaluations/${evaluationID}/decision`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ decision, final_payload: finalPayload, decision_note: decisionNote }),
  }, "处理评估结果失败");
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
  | "queue_paused";

export interface FeynmanPracticeState {
  state: FeynmanPracticeStateName;
  question: string;
  round_no: number;
}

export function getFeynmanPracticeState(sessionID: string): Promise<FeynmanPracticeState> {
  return request(
    `/feynman/practice-state?session_id=${encodeURIComponent(sessionID)}`,
    { method: "GET" },
    "查询练习状态失败",
  );
}
