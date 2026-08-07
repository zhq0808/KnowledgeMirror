/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import {
  ChatStreamError,
  consumeChatSSEStream,
  parseChatSSEFrame,
} from "./chat.ts";

function streamOf(...chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

test("parses numeric SSE business error code and message", () => {
  const frame = parseChatSSEFrame(
    'event: error\ndata: {"code":40912,"message":"教练任务ID与当前练习不匹配"}',
  );
  assert.equal(frame.event, "error");
  assert.equal(frame.code, 40912);
  assert.equal(frame.message, "教练任务ID与当前练习不匹配");
});

test("throws non-retryable typed error for a Coach conflict", async () => {
  await assert.rejects(
    consumeChatSSEStream(
      streamOf('event: error\ndata: {"code":40911,"message":"必须携带教练任务ID","retryable":false}\n\n'),
      { onDelta() {}, onSources() {} },
    ),
    (error: unknown) =>
      error instanceof ChatStreamError &&
      error.code === 40911 &&
      !error.retryable &&
      error.message === "必须携带教练任务ID",
  );
});

test("EOF before done is a typed retryable stream error", async () => {
  const deltas: string[] = [];
  await assert.rejects(
    consumeChatSSEStream(streamOf('data: {"delta":"半段回复"}\n\n'), {
      onDelta: (delta) => deltas.push(delta),
      onSources() {},
    }),
    (error: unknown) =>
      error instanceof ChatStreamError && error.retryable && /意外中断/.test(error.message),
  );
  assert.deepEqual(deltas, ["半段回复"]);
});

test("done ends the stream and returns the practice projection", async () => {
  let coachTaskID = "";
  await consumeChatSSEStream(
    streamOf(
      'data: {"delta":"完成"}\n\n',
      'event: done\ndata: {"feynman":{"state":"awaiting_answer","question":"原题","round_no":0,"coach_task_id":"task-1","original_question":"原题","retry_required":false}}\n\n',
    ),
    {
      onDelta() {},
      onSources() {},
      onPracticeState: (state) => {
        coachTaskID = state.coach_task_id;
      },
    },
  );
  assert.equal(coachTaskID, "task-1");
});
