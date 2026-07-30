import { StreamingPCMEncoder } from "./pcm-capture";

declare const sampleRate: number;

declare abstract class AudioWorkletProcessor {
  readonly port: MessagePort;
  abstract process(inputs: Float32Array[][]): boolean;
}

declare function registerProcessor(
  name: string,
  processor: new () => AudioWorkletProcessor,
): void;

type CaptureCommand = { type: "stop" };

class PCMCaptureProcessor extends AudioWorkletProcessor {
  private readonly encoder = new StreamingPCMEncoder(sampleRate);
  private stopped = false;

  constructor() {
    super();
    this.port.onmessage = (event: MessageEvent<CaptureCommand>) => {
      if (event.data?.type !== "stop" || this.stopped) return;
      this.stopped = true;
      this.postFrames(this.encoder.flush());
      this.port.postMessage({ type: "stopped" });
    };
  }

  process(inputs: Float32Array[][]): boolean {
    if (this.stopped) return false;
    const channels = inputs[0];
    if (channels?.length) this.postFrames(this.encoder.push(channels));
    return true;
  }

  private postFrames(frames: Uint8Array[]): void {
    for (const frame of frames) {
      const buffer = frame.buffer;
      this.port.postMessage({ type: "frame", buffer }, [buffer]);
    }
  }
}

registerProcessor("pcm-capture", PCMCaptureProcessor);
