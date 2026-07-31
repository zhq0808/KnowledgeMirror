/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import { realtimeVoiceWebSocketURL } from "../api/realtimeVoice.ts";
import type { VoiceCapture } from "../api/voice.ts";
import {
  RealtimeTranscriptionController,
  type RealtimeCapture,
  type RealtimeCaptureOptions,
  type RealtimeTranscriptionDependencies,
  type RealtimeTranscriptionStatus,
  type RealtimeVoiceSocket,
} from "./realtime-transcription.ts";

class FakeSocket implements RealtimeVoiceSocket {
  binaryType: BinaryType = "blob";
  readyState = 1;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  readonly sent: Array<string | ArrayBuffer> = [];
  closeCount = 0;

  send(data: string | ArrayBuffer): void {
    this.sent.push(data);
  }

  close(): void {
    this.closeCount += 1;
    this.readyState = 3;
  }

  emit(event: object): void {
    this.onmessage?.({ data: JSON.stringify(event) });
  }

  disconnect(): void {
    this.readyState = 3;
    this.onclose?.();
  }
}

class FakeCapture implements RealtimeCapture {
  stopCount = 0;
  disposeCount = 0;
  private readonly options: RealtimeCaptureOptions;
  private readonly tailFrame: ArrayBuffer;

  constructor(options: RealtimeCaptureOptions, tailFrame = new Uint8Array([9, 0]).buffer) {
    this.options = options;
    this.tailFrame = tailFrame;
  }

  async stop(): Promise<void> {
    this.stopCount += 1;
    this.options.onFrame(this.tailFrame);
  }

  dispose(): void {
    this.disposeCount += 1;
  }
}

function createRig() {
  const sockets: FakeSocket[] = [];
  const captures: FakeCapture[] = [];
  const captureOptions: RealtimeCaptureOptions[] = [];
  const urls: string[] = [];
  const dependencies: RealtimeTranscriptionDependencies = {
    createURL: (sessionID) => `ws://example.test/api/v1/voice/realtime?session_id=${sessionID}`,
    createSocket: (url) => {
      urls.push(url);
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    startCapture: async (options) => {
      captureOptions.push(options);
      const capture = new FakeCapture(options);
      captures.push(capture);
      return capture;
    },
  };
  return {
    controller: new RealtimeTranscriptionController(dependencies),
    sockets,
    captures,
    captureOptions,
    urls,
  };
}

async function readyRig(rig: ReturnType<typeof createRig>, prefix = ""): Promise<FakeSocket> {
  rig.controller.start("session-1", prefix);
  const socket = rig.sockets[0];
  socket.emit({ type: "ready", stream_id: "stream-1", sample_rate: 16000 });
  await settle();
  return socket;
}

function transcript(
  socket: FakeSocket,
  seq: number,
  sentenceID: number,
  text: string,
  sentenceEnd = false,
): void {
  socket.emit({
    type: "transcript",
    seq,
    sentence_id: sentenceID,
    text,
    sentence_end: sentenceEnd,
    begin_time_ms: 0,
    end_time_ms: sentenceEnd ? 100 : null,
  });
}

function completedCapture(transcriptText: string): VoiceCapture {
  return {
    capture_id: "capture-1",
    session_id: "session-1",
    status: "transcribed",
    transcript: transcriptText,
    confidence: null,
    ambiguous_terms: [],
    needs_confirmation: true,
    confirmation_reason: "missing_confidence",
    created_at: "2026-07-30T00:00:00Z",
  };
}

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

test("builds a same-origin secure WebSocket URL with an encoded session ID", () => {
  const url = realtimeVoiceWebSocketURL("session /?", {
    protocol: "https:",
    host: "knowledge.example:8443",
  });
  assert.equal(
    url,
    "wss://knowledge.example:8443/api/v1/voice/realtime?session_id=session+%2F%3F",
  );
});

test("waits for ready, streams frames in order, flushes before stop, then waits for completed", async () => {
  const rig = createRig();
  const statuses: RealtimeTranscriptionStatus[] = [];
  rig.controller.subscribe((snapshot) => statuses.push(snapshot.status));

  rig.controller.start("session with space", "补充：");
  const socket = rig.sockets[0];
  assert.equal(rig.captureOptions.length, 0);
  assert.equal(rig.controller.snapshot.status, "connecting");

  socket.emit({ type: "ready", stream_id: "stream-1", sample_rate: 16000 });
  assert.equal(rig.controller.snapshot.status, "ready");
  await settle();
  assert.equal(rig.controller.snapshot.status, "streaming");
  assert.equal(rig.captureOptions.length, 1);

  const first = new Uint8Array([1, 0]).buffer;
  const second = new Uint8Array([2, 0]).buffer;
  rig.captureOptions[0].onFrame(first);
  rig.captureOptions[0].onFrame(second);

  await rig.controller.stop();
  assert.equal(rig.controller.snapshot.status, "stopping");
  assert.deepEqual(socket.sent, [first, second, new Uint8Array([9, 0]).buffer, '{"type":"stop"}']);

  transcript(socket, 1, 1, "最后一句。", true);
  socket.emit({ type: "completed", capture: completedCapture("服务端最终文本。") });
  assert.equal(rig.controller.snapshot.status, "review");
  assert.equal(rig.controller.snapshot.text, "补充：服务端最终文本。");
  assert.equal(rig.controller.snapshot.voiceCaptureID, "capture-1");
  assert.equal(socket.closeCount, 1);
  assert.deepEqual(statuses, ["idle", "connecting", "ready", "streaming", "stopping", "stopping", "review"]);
});

test("interim replaces by sentence, seq never moves backwards, and committed finals cannot regress", async () => {
  const rig = createRig();
  const socket = await readyRig(rig, "前缀：");

  transcript(socket, 1, 1, "Kafka");
  transcript(socket, 2, 1, "Kafka 幂等");
  transcript(socket, 2, 1, "重复事件不应生效");
  transcript(socket, 1, 1, "乱序事件不应生效");
  assert.equal(rig.controller.snapshot.text, "前缀：Kafka 幂等");

  transcript(socket, 3, 1, "Kafka 幂等。", true);
  transcript(socket, 4, 1, "final 后不能回退");
  transcript(socket, 5, 2, "第二句过程");
  assert.equal(rig.controller.snapshot.text, "前缀：Kafka 幂等。第二句过程");
  assert.equal(rig.controller.snapshot.committedSentences.length, 1);
  assert.equal(rig.controller.snapshot.committedSentences[0], "Kafka 幂等。");
  assert.equal(rig.controller.snapshot.interim, "第二句过程");

  transcript(socket, 6, 2, "第二句定稿。", true);
  assert.equal(rig.controller.snapshot.text, "前缀：Kafka 幂等。第二句定稿。");
  assert.equal(rig.controller.snapshot.committedSentences.length, 2);
  assert.equal(rig.controller.snapshot.committedSentences[0], "Kafka 幂等。");
  assert.equal(rig.controller.snapshot.committedSentences[1], "第二句定稿。");
  assert.equal(rig.controller.snapshot.interim, "");
});

test("upstream errors preserve visible text and clear the capture association", async () => {
  const rig = createRig();
  const socket = await readyRig(rig, "已有输入：");
  transcript(socket, 1, 1, "暂存文字");

  socket.emit({ type: "error", code: "upstream_failed", message: "上游识别失败", retryable: true });
  assert.equal(rig.controller.snapshot.status, "failed");
  assert.equal(rig.controller.snapshot.text, "已有输入：暂存文字");
  assert.equal(rig.controller.snapshot.voiceCaptureID, null);
  assert.equal(rig.controller.snapshot.error, "上游识别失败");
  assert.equal(rig.controller.snapshot.retryable, true);
  assert.equal(rig.captures[0].disposeCount, 1);
  assert.equal(socket.closeCount, 1);
});

test("unexpected network close fails without discarding the latest interim", async () => {
  const rig = createRig();
  const socket = await readyRig(rig);
  transcript(socket, 1, 1, "断网前的文字");

  socket.disconnect();
  assert.equal(rig.controller.snapshot.status, "failed");
  assert.equal(rig.controller.snapshot.text, "断网前的文字");
  assert.equal(rig.controller.snapshot.voiceCaptureID, null);
});

test("microphone permission denial closes the socket and exposes a retryable-safe UI error", async () => {
  const rig = createRig();
  rig.controller = new RealtimeTranscriptionController({
    createURL: (sessionID) => `ws://example.test/api/v1/voice/realtime?session_id=${sessionID}`,
    createSocket: (url) => {
      rig.urls.push(url);
      const socket = new FakeSocket();
      rig.sockets.push(socket);
      return socket;
    },
    startCapture: async () => {
      throw new Error("麦克风权限被拒绝，请在浏览器设置中允许后重试");
    },
  });

  rig.controller.start("session-1", "保留前缀：");
  const socket = rig.sockets[0];
  socket.emit({ type: "ready", stream_id: "stream-1", sample_rate: 16000 });
  await settle();

  assert.equal(rig.controller.snapshot.status, "failed");
  assert.equal(rig.controller.snapshot.text, "保留前缀：");
  assert.match(rig.controller.snapshot.error, /麦克风权限被拒绝/);
  assert.equal(rig.controller.snapshot.voiceCaptureID, null);
  assert.equal(socket.closeCount, 1);
});

test("reset for a session switch releases the old stream and removes its transcript association", async () => {
  const rig = createRig();
  const oldSocket = await readyRig(rig, "A 会话：");
  transcript(oldSocket, 1, 1, "不能带到 B 会话");

  rig.controller.reset("");

  assert.equal(rig.controller.snapshot.status, "idle");
  assert.equal(rig.controller.snapshot.text, "");
  assert.equal(rig.controller.snapshot.voiceCaptureID, null);
  assert.equal(rig.captures[0].disposeCount, 1);
  assert.equal(oldSocket.closeCount, 1);
});

test("dispose on component unmount releases microphone and WebSocket resources", async () => {
  const rig = createRig();
  const socket = await readyRig(rig);

  rig.controller.dispose();
  assert.equal(rig.captures[0].disposeCount, 1);
  assert.equal(socket.closeCount, 1);

  socket.emit({ type: "error", code: "late", message: "late event", retryable: false });
  assert.equal(rig.controller.snapshot.status, "streaming");
});