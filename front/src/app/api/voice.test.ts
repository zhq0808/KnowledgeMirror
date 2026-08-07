/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import { uploadVoiceCapture, type VoiceCapture } from "./voice.ts";

const originalFetch = globalThis.fetch;

test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("uploadVoiceCapture posts a WAV multipart payload with session and rounded duration", async () => {
  const capture: VoiceCapture = {
    capture_id: "capture-1",
    session_id: "session-1",
    status: "transcribed",
    transcript: "测试",
    confidence: null,
    ambiguous_terms: [],
    needs_confirmation: true,
    confirmation_reason: "missing_confidence",
    created_at: "2026-08-07T00:00:00Z",
  };
  let request: { input: string | URL | Request; init?: RequestInit } | null = null;
  globalThis.fetch = async (input, init) => {
    request = { input, init };
    return new Response(JSON.stringify({ code: 0, message: "ok", data: capture }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };

  const result = await uploadVoiceCapture("session-1", new Blob([Uint8Array.of(1, 2)], { type: "audio/wav" }), 1234.6);

  assert.equal(result.capture_id, "capture-1");
  assert.equal(request?.input, "/api/v1/voice/captures");
  assert.equal(request?.init?.method, "POST");
  assert.equal(request?.init?.credentials, "include");
  const form = request?.init?.body as FormData;
  assert.equal(form.get("session_id"), "session-1");
  assert.equal(form.get("duration_ms"), "1235");
  const audio = form.get("audio") as File;
  assert.equal(audio.name, "voice.wav");
  assert.equal(audio.type, "audio/wav");
});

test("uploadVoiceCapture surfaces the backend message on capability or service failure", async () => {
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ code: 50301, message: "录音上传转写服务未配置" }), {
      status: 503,
      headers: { "content-type": "application/json" },
    });

  await assert.rejects(
    uploadVoiceCapture("session-1", new Blob([Uint8Array.of(1, 2)], { type: "audio/wav" }), 100),
    /录音上传转写服务未配置/,
  );
});
