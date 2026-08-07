export const PCM_TARGET_SAMPLE_RATE = 16000;
export const PCM_FRAME_BYTES = 3200;
export const PCM16_WAV_MIME_TYPE = "audio/wav";

export function mixToMono(channels: readonly Float32Array[]): Float32Array {
  if (channels.length === 0) return new Float32Array(0);

  const sampleCount = Math.min(...channels.map((channel) => channel.length));
  const mono = new Float32Array(sampleCount);
  for (const channel of channels) {
    for (let index = 0; index < sampleCount; index += 1) {
      mono[index] += channel[index] / channels.length;
    }
  }
  return mono;
}

export class StreamingLinearResampler {
  private readonly sourceSampleRate: number;
  private readonly targetSampleRate: number;
  private pending: Float32Array<ArrayBufferLike> = new Float32Array(0);
  private pendingStart = 0;
  private nextOutputIndex = 0;

  constructor(sourceSampleRate: number, targetSampleRate: number = PCM_TARGET_SAMPLE_RATE) {
    if (sourceSampleRate <= 0 || targetSampleRate <= 0) {
      throw new RangeError("sample rates must be positive");
    }
    this.sourceSampleRate = sourceSampleRate;
    this.targetSampleRate = targetSampleRate;
  }

  push(samples: Float32Array): Float32Array {
    if (samples.length === 0) return new Float32Array(0);
    this.pending = concatFloat32(this.pending, samples);
    return this.readAvailable(false);
  }

  flush(): Float32Array {
    const output = this.readAvailable(true);
    this.pending = new Float32Array(0);
    this.pendingStart = 0;
    this.nextOutputIndex = 0;
    return output;
  }

  private readAvailable(flushing: boolean): Float32Array {
    const output: number[] = [];
    const receivedEnd = this.pendingStart + this.pending.length;

    while (true) {
      const sourcePosition =
        (this.nextOutputIndex * this.sourceSampleRate) / this.targetSampleRate;
      const absoluteLeft = Math.floor(sourcePosition);
      const hasSample = flushing
        ? absoluteLeft < receivedEnd
        : absoluteLeft + 1 < receivedEnd;
      if (!hasSample) break;

      const leftIndex = absoluteLeft - this.pendingStart;
      const rightIndex = Math.min(leftIndex + 1, this.pending.length - 1);
      const fraction = sourcePosition - absoluteLeft;
      output.push(
        this.pending[leftIndex] +
          (this.pending[rightIndex] - this.pending[leftIndex]) * fraction,
      );
      this.nextOutputIndex += 1;
    }

    const nextSourcePosition =
      (this.nextOutputIndex * this.sourceSampleRate) / this.targetSampleRate;
    const consumed = Math.min(
      Math.max(Math.floor(nextSourcePosition) - this.pendingStart, 0),
      this.pending.length,
    );
    if (consumed > 0) {
      this.pending = this.pending.slice(consumed);
      this.pendingStart += consumed;
    }
    return Float32Array.from(output);
  }
}

export function float32ToPCM16LE(samples: Float32Array): Uint8Array {
  const pcm = new Uint8Array(samples.length * 2);
  const view = new DataView(pcm.buffer);
  for (let index = 0; index < samples.length; index += 1) {
    const sample = Number.isFinite(samples[index]) ? samples[index] : 0;
    const clamped = Math.max(-1, Math.min(1, sample));
    const value = clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff;
    view.setInt16(index * 2, value, true);
  }
  return pcm;
}

export function encodePCM16LEWav(
  chunks: readonly Uint8Array[],
  sampleRate: number = PCM_TARGET_SAMPLE_RATE,
): Blob {
  if (sampleRate <= 0) throw new RangeError("sampleRate must be positive");

  const dataBytes = chunks.reduce((total, chunk) => {
    if (chunk.byteLength % 2 !== 0) {
      throw new RangeError("PCM16 data must contain an even number of bytes");
    }
    return total + chunk.byteLength;
  }, 0);
  const header = new ArrayBuffer(44);
  const view = new DataView(header);

  writeAscii(view, 0, "RIFF");
  view.setUint32(4, 36 + dataBytes, true);
  writeAscii(view, 8, "WAVE");
  writeAscii(view, 12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  writeAscii(view, 36, "data");
  view.setUint32(40, dataBytes, true);

  return new Blob([header, ...chunks], { type: PCM16_WAV_MIME_TYPE });
}

export class PCMFrameAccumulator {
  private pending: Uint8Array<ArrayBufferLike> = new Uint8Array(0);
  private readonly frameBytes: number;

  constructor(frameBytes: number = PCM_FRAME_BYTES) {
    if (frameBytes <= 0 || frameBytes % 2 !== 0) {
      throw new RangeError("frameBytes must be a positive even number");
    }
    this.frameBytes = frameBytes;
  }

  push(bytes: Uint8Array): Uint8Array[] {
    if (bytes.length % 2 !== 0) {
      throw new RangeError("PCM16 data must contain an even number of bytes");
    }
    this.pending = concatUint8(this.pending, bytes);

    const frames: Uint8Array[] = [];
    while (this.pending.length >= this.frameBytes) {
      frames.push(this.pending.slice(0, this.frameBytes));
      this.pending = this.pending.slice(this.frameBytes);
    }
    return frames;
  }

  flush(): Uint8Array | null {
    if (this.pending.length === 0) return null;
    const tail = this.pending;
    this.pending = new Uint8Array(0);
    return tail;
  }
}

export class StreamingPCMEncoder {
  private readonly resampler: StreamingLinearResampler;
  private readonly frames: PCMFrameAccumulator;

  constructor(sourceSampleRate: number) {
    this.resampler = new StreamingLinearResampler(sourceSampleRate);
    this.frames = new PCMFrameAccumulator();
  }

  push(channels: readonly Float32Array[]): Uint8Array[] {
    const mono = mixToMono(channels);
    return this.frames.push(float32ToPCM16LE(this.resampler.push(mono)));
  }

  flush(): Uint8Array[] {
    const frames = this.frames.push(float32ToPCM16LE(this.resampler.flush()));
    const tail = this.frames.flush();
    if (tail) frames.push(tail);
    return frames;
  }
}

function writeAscii(view: DataView, offset: number, text: string): void {
  for (let index = 0; index < text.length; index += 1) {
    view.setUint8(offset + index, text.charCodeAt(index));
  }
}

function concatFloat32(left: Float32Array, right: Float32Array): Float32Array {
  const combined = new Float32Array(left.length + right.length);
  combined.set(left);
  combined.set(right, left.length);
  return combined;
}

function concatUint8(left: Uint8Array, right: Uint8Array): Uint8Array {
  const combined = new Uint8Array(left.length + right.length);
  combined.set(left);
  combined.set(right, left.length);
  return combined;
}
