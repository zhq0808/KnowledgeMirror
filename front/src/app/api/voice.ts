// voice.ts 定义语音记录类型，并封装录音上传转写接口。

import { request } from "./knowledge.ts";

// VoiceAmbiguousTerm 是一条术语歧义提示：Term 是词表里的标准写法，Heard 是转写里出现的可疑写法。
// 它只是提示，不代表判定用户讲错了。
export interface VoiceAmbiguousTerm {
  term: string;
  heard: string;
}

export interface VoiceCapture {
  capture_id: string;
  session_id: string;
  status: "uploaded" | "transcribing" | "transcribed" | "failed";
  transcript: string;
  confidence: number | null;
  ambiguous_terms: VoiceAmbiguousTerm[];
  needs_confirmation: boolean;
  confirmation_reason:
    | ""
    | "transcribe_failed"
    | "missing_confidence"
    | "low_confidence"
    | "ambiguous_terms";
  transcript_error?: string;
  duration_ms?: number;
  created_at: string;
}

// alwaysConfirmKey 记住用户「每次都先确认」的个人偏好。
// 后端只按客观证据裁决要不要确认；用户想更谨慎是他自己的选择，存在本地即可。
const alwaysConfirmKey = "voice_always_confirm";

export function getVoiceAlwaysConfirm(): boolean {
  return localStorage.getItem(alwaysConfirmKey) === "1";
}

export function setVoiceAlwaysConfirm(enabled: boolean): void {
  if (enabled) {
    localStorage.setItem(alwaysConfirmKey, "1");
  } else {
    localStorage.removeItem(alwaysConfirmKey);
  }
}

export function voiceConfirmationHint(capture: VoiceCapture): string {
  switch (capture.confirmation_reason) {
    case "transcribe_failed":
      return capture.transcript_error || "没能听清这段话，可以重录或直接打字";
    case "low_confidence":
    case "missing_confidence":
      return "这段转写可能不太准，发送前先看一眼";
    case "ambiguous_terms": {
      const terms = capture.ambiguous_terms
        .map((item) => `${item.heard} → ${item.term}?`)
        .join("、");
      return terms ? `这些词可能被听错了：${terms}` : "有术语可能被听错了，发送前先看一眼";
    }
    default:
      return "";
  }
}

export async function uploadVoiceCapture(
  sessionID: string,
  audio: Blob,
  durationMs: number,
): Promise<VoiceCapture> {
  const form = new FormData();
  form.append("audio", audio, "voice.wav");
  form.append("session_id", sessionID);
  form.append("duration_ms", String(Math.max(1, Math.round(durationMs))));

  return request<VoiceCapture>(
    "/voice/captures",
    { method: "POST", body: form },
    "语音转写失败",
  );
}

