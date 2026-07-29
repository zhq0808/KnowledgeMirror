// voice.ts 封装通用语音输入接口。
//
// 这里只做「把录音变成文字」。转写完成后文字回到输入框，用户按发送走的仍是
// /api/v1/chat/stream —— 语音是输入法，不是第二条业务链路。

import { audioFileExtension } from "../lib/audio";
import { APIError } from "./knowledge";

const basePath = "/api/v1";

interface APIResponse<T> {
  code: number;
  message: string;
  data?: T;
}

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

// voiceConfirmationHint 把后端给出的确认原因翻译成一句用户能懂的话。
// 不写「置信度 0.42」这种数字：用户要的是「要不要看一眼」，不是模型内部指标。
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

// uploadVoiceCapture 上传一段录音并同步拿回转写。
// 转写失败时后端同样返回 200 + status=failed（录音这件事确实发生了），
// 因此调用方要看 status，不能只靠 try/catch 判断成败。
export async function uploadVoiceCapture(
  sessionID: string,
  audio: Blob,
  durationMs: number
): Promise<VoiceCapture> {
  const extension = audioFileExtension(audio.type);
  const form = new FormData();
  form.append("audio", audio, `voice.${extension}`);
  form.append("session_id", sessionID);
  form.append("duration_ms", String(Math.max(1, Math.round(durationMs))));

  let response: Response;
  try {
    response = await fetch(`${basePath}/voice/captures`, {
      method: "POST",
      credentials: "include",
      body: form,
    });
  } catch {
    throw new APIError("网络异常，请检查连接后重试", 0, -1);
  }
  let body: APIResponse<VoiceCapture> | null = null;
  try {
    body = (await response.json()) as APIResponse<VoiceCapture>;
  } catch {
    body = null;
  }
  if (!response.ok || !body || body.code !== 0 || !body.data) {
    throw new APIError(body?.message || "语音转写失败", response.status, body?.code ?? -1);
  }
  return body.data;
}
