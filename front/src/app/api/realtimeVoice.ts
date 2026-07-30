import type { VoiceCapture } from "./voice";

export interface RealtimeVoiceReadyEvent {
  type: "ready";
  stream_id: string;
  sample_rate: number;
}

export interface RealtimeVoiceTranscriptEvent {
  type: "transcript";
  seq: number;
  sentence_id: number;
  text: string;
  sentence_end: boolean;
  begin_time_ms: number;
  end_time_ms?: number | null;
}

export interface RealtimeVoiceCompletedEvent {
  type: "completed";
  capture: VoiceCapture;
}

export interface RealtimeVoiceErrorEvent {
  type: "error";
  code: string;
  message: string;
  retryable: boolean;
}

export type RealtimeVoiceServerEvent =
  | RealtimeVoiceReadyEvent
  | RealtimeVoiceTranscriptEvent
  | RealtimeVoiceCompletedEvent
  | RealtimeVoiceErrorEvent;

export interface RealtimeVoiceStopControl {
  type: "stop";
}

interface WebSocketLocation {
  protocol: string;
  host: string;
}

export function realtimeVoiceWebSocketURL(
  sessionID: string,
  currentLocation: WebSocketLocation = window.location,
): string {
  const protocol = currentLocation.protocol === "https:" ? "wss:" : "ws:";
  const url = new URL("/api/v1/voice/realtime", `${protocol}//${currentLocation.host}`);
  url.searchParams.set("session_id", sessionID);
  return url.toString();
}

export function parseRealtimeVoiceEvent(raw: unknown): RealtimeVoiceServerEvent {
  if (typeof raw !== "string") throw new Error("实时转写服务返回了无法识别的数据");

  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error("实时转写服务返回了无效消息");
  }
  if (!isRecord(value) || typeof value.type !== "string") {
    throw new Error("实时转写服务返回了无效消息");
  }

  switch (value.type) {
    case "ready":
      if (typeof value.stream_id === "string" && isFiniteNumber(value.sample_rate)) {
        return value as unknown as RealtimeVoiceReadyEvent;
      }
      break;
    case "transcript":
      if (
        isFiniteNumber(value.seq) &&
        isFiniteNumber(value.sentence_id) &&
        typeof value.text === "string" &&
        typeof value.sentence_end === "boolean" &&
        isFiniteNumber(value.begin_time_ms) &&
        (value.end_time_ms === undefined || value.end_time_ms === null || isFiniteNumber(value.end_time_ms))
      ) {
        return value as unknown as RealtimeVoiceTranscriptEvent;
      }
      break;
    case "completed":
      if (isVoiceCapture(value.capture)) return value as unknown as RealtimeVoiceCompletedEvent;
      break;
    case "error":
      if (
        typeof value.code === "string" &&
        typeof value.message === "string" &&
        typeof value.retryable === "boolean"
      ) {
        return value as unknown as RealtimeVoiceErrorEvent;
      }
      break;
  }
  throw new Error("实时转写服务返回了无效消息");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

function isVoiceCapture(value: unknown): value is VoiceCapture {
  return (
    isRecord(value) &&
    typeof value.capture_id === "string" &&
    typeof value.session_id === "string" &&
    value.status === "transcribed" &&
    typeof value.transcript === "string" &&
    (value.confidence === null || isFiniteNumber(value.confidence)) &&
    Array.isArray(value.ambiguous_terms) &&
    typeof value.needs_confirmation === "boolean" &&
    typeof value.confirmation_reason === "string" &&
    typeof value.created_at === "string"
  );
}