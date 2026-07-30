/// <reference types="node" />

import assert from "node:assert/strict";
import test from "node:test";

import {
  float32ToPCM16LE,
  mixToMono,
  PCMFrameAccumulator,
  StreamingPCMEncoder,
} from "./pcm-capture.ts";

function encodeOneSecond(sourceSampleRate: number): Uint8Array[] {
  const encoder = new StreamingPCMEncoder(sourceSampleRate);
  const samples = Float32Array.from(
    { length: sourceSampleRate },
    (_, index) => Math.sin((2 * Math.PI * 440 * index) / sourceSampleRate),
  );
  const frames: Uint8Array[] = [];
  for (let offset = 0; offset < samples.length; offset += 128) {
    frames.push(...encoder.push([samples.subarray(offset, offset + 128)]));
  }
  frames.push(...encoder.flush());
  return frames;
}

for (const sourceSampleRate of [48000, 44100]) {
  test(`${sourceSampleRate} Hz resamples one second to 32000 PCM bytes`, () => {
    const frames = encodeOneSecond(sourceSampleRate);
    assert.equal(frames.reduce((total, frame) => total + frame.length, 0), 32000);
    assert.deepEqual(frames.map((frame) => frame.length), Array(10).fill(3200));
  });
}

test("Float32 conversion saturates and writes little-endian PCM16", () => {
  const pcm = float32ToPCM16LE(Float32Array.of(-2, -1, -0.5, 0, 0.5, 1, 2, Number.NaN));
  const view = new DataView(pcm.buffer);
  assert.deepEqual(
    Array.from({ length: pcm.length / 2 }, (_, index) => view.getInt16(index * 2, true)),
    [-32768, -32768, -16384, 0, 16383, 32767, 32767, 0],
  );
});

test("multiple input channels are averaged into mono", () => {
  const mono = mixToMono([Float32Array.of(1, -1), Float32Array.of(-1, 0.5)]);
  assert.deepEqual(Array.from(mono), [0, -0.25]);
});

test("frame accumulator emits 3200-byte frames and flushes an even tail", () => {
  const accumulator = new PCMFrameAccumulator();
  const frames = accumulator.push(new Uint8Array(3360));
  assert.deepEqual(frames.map((frame) => frame.length), [3200]);
  assert.equal(accumulator.flush()?.length, 160);
  assert.equal(accumulator.flush(), null);
});

test("128-sample render quanta stay buffered until 100 ms is complete", () => {
  const encoder = new StreamingPCMEncoder(48000);
  for (let quantum = 0; quantum < 37; quantum += 1) {
    assert.deepEqual(encoder.push([new Float32Array(128)]), []);
  }
  assert.deepEqual(encoder.push([new Float32Array(64)]).map((frame) => frame.length), [3200]);
  assert.deepEqual(encoder.flush(), []);
});

test("streaming encoder keeps the final short frame until flush", () => {
  const encoder = new StreamingPCMEncoder(48000);
  const frames = encoder.push([new Float32Array(5040)]);
  assert.deepEqual(frames.map((frame) => frame.length), [3200]);
  const tail = encoder.flush();
  assert.deepEqual(tail.map((frame) => frame.length), [160]);
  assert.equal(tail[0].length % 2, 0);
});
