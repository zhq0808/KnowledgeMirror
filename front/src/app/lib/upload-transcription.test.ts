/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import type { VoiceCapture } from "../api/voice.ts";
import {
  UploadTranscriptionController,
  type UploadCapture,
  type UploadCaptureOptions,
} from "./upload-transcription.ts";
import { selectVoiceInputMode } from "./voice-mode.ts";

class FakeCapture implements UploadCapture {
  stopCount = 0;
  disposeCount = 0;
  private readonly options: UploadCaptureOptions;

  constructor(options: UploadCaptureOptions) {
    this.options = options;
  }

  emit(...bytes: number[]): void {
    this.options.onFrame(Uint8Array.from(bytes).buffer);
  }

  async stop(): Promise<void> {
    this.stopCount += 1;
    this.emit(3, 0);
  }

  dispose(): void {
    this.disposeCount += 1;
  }
}

function transcribedCapture(): VoiceCapture {
  return {
    capture_id: "capture-1",
    session_id: "session-1",
    status: "transcribed",
    transcript: "上传转写结果",
    confidence: 0.9,
    ambiguous_terms: [],
    needs_confirmation: false,
    confirmation_reason: "",
    created_at: "2026-08-07T00:00:00Z",
  };
}

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

test("voice mode prefers realtime and falls back to file upload", () => {
  assert.equal(selectVoiceInputMode(true, true), "realtime");
  assert.equal(selectVoiceInputMode(false, true), "upload");
  assert.equal(selectVoiceInputMode(false, false), null);
});

test("records PCM frames, stops capture, builds WAV, then exposes uploaded transcript", async () => {
  let now = 1000;
  let capture: FakeCapture | null = null;
  let uploaded: { sessionID: string; audio: Blob; durationMs: number } | null = null;
  const controller = new UploadTranscriptionController({
    now: () => now,
    startCapture: async (options) => {
      capture = new FakeCapture(options);
      return capture;
    },
    upload: async (sessionID, audio, durationMs) => {
      uploaded = { sessionID, audio, durationMs };
      return transcribedCapture();
    },
  });

  controller.start("session-1", "已有输入：");
  now = 5000;
  await settle();
  assert.equal(controller.snapshot.status, "recording");
  assert.equal(controller.snapshot.elapsedMs, 0);
  capture?.emit(1, 0, 2, 0);
  now = 6250;
  await controller.stop();

  assert.equal(capture?.stopCount, 1);
  assert.equal(capture?.disposeCount, 1);
  assert.equal(uploaded?.sessionID, "session-1");
  assert.equal(uploaded?.durationMs, 1250);
  assert.equal(uploaded?.audio.type, "audio/wav");
  const bytes = new Uint8Array(await uploaded!.audio.arrayBuffer());
  assert.equal(new TextDecoder().decode(bytes.subarray(0, 4)), "RIFF");
  assert.deepEqual(Array.from(bytes.subarray(44)), [1, 0, 2, 0, 3, 0]);
  assert.equal(controller.snapshot.status, "review");
  assert.equal(controller.snapshot.text, "已有输入：上传转写结果");
  assert.equal(controller.snapshot.voiceCaptureID, "capture-1");
});

test("uploaded duration excludes permission delay and is capped at the recording limit", async () => {
  let now = 0;
  let capture: FakeCapture | null = null;
  let durationMs = 0;
  const controller = new UploadTranscriptionController({
    now: () => now,
    startCapture: async (options) => {
      capture = new FakeCapture(options);
      return capture;
    },
    upload: async (_sessionID, _audio, duration) => {
      durationMs = duration;
      return transcribedCapture();
    },
  });

  controller.start("session-1");
  now = 30000;
  await settle();
  capture?.emit(1, 0);
  now = 30000 + 181000;
  await controller.stop();
  assert.equal(durationMs, 180000);
});

test("capture and upload failures preserve typed text and expose actionable errors", async () => {
  const permissionController = new UploadTranscriptionController({
    startCapture: async () => {
      throw new Error("麦克风权限被拒绝，请在浏览器设置中允许后重试");
    },
    upload: async () => transcribedCapture(),
  });
  permissionController.start("session-1", "保留文字");
  await settle();
  assert.equal(permissionController.snapshot.status, "failed");
  assert.equal(permissionController.snapshot.text, "保留文字");
  assert.match(permissionController.snapshot.error, /麦克风权限被拒绝/);

  let capture: FakeCapture | null = null;
  const uploadController = new UploadTranscriptionController({
    startCapture: async (options) => {
      capture = new FakeCapture(options);
      return capture;
    },
    upload: async () => {
      throw new Error("网络异常，请检查连接后重试");
    },
  });
  uploadController.start("session-1", "仍保留：");
  await settle();
  capture?.emit(1, 0);
  await uploadController.stop("session-1");
  assert.equal(uploadController.snapshot.status, "failed");
  assert.equal(uploadController.snapshot.text, "仍保留：");
  assert.match(uploadController.snapshot.error, /网络异常/);
  assert.equal(uploadController.snapshot.voiceCaptureID, null);
});
