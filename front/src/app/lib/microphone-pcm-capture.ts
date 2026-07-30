import pcmCaptureWorkletURL from "./pcm-capture.worklet.ts?worker&url";

const STOP_TIMEOUT_MS = 2000;

type AudioContextConstructor = new (contextOptions?: AudioContextOptions) => AudioContext;

interface LegacyAudioWindow {
  webkitAudioContext?: AudioContextConstructor;
}

interface WorkletFrameMessage {
  type: "frame";
  buffer: ArrayBuffer;
}

interface WorkletStoppedMessage {
  type: "stopped";
}

type WorkletMessage = WorkletFrameMessage | WorkletStoppedMessage;

export interface MicrophonePCMCaptureOptions {
  onFrame: (pcm16LE: ArrayBuffer) => void;
  onError?: (error: Error) => void;
  signal?: AbortSignal;
}

export function realtimeCaptureUnavailableReason(): string | null {
  if (typeof window === "undefined" || typeof navigator === "undefined") {
    return "当前环境不支持实时录音";
  }
  if (!navigator.mediaDevices?.getUserMedia) {
    return window.isSecureContext
      ? "当前浏览器不支持麦克风采集，请使用最新版 Chrome、Edge 或 Safari"
      : "浏览器只在 HTTPS、localhost 或 127.0.0.1 下开放麦克风";
  }

  const AudioContextCtor = getAudioContextConstructor();
  if (!AudioContextCtor || !("AudioWorkletNode" in window)) {
    return "当前浏览器不支持实时音频采集，请使用最新版 Chrome、Edge 或 Safari";
  }
  return null;
}

export class MicrophonePCMCapture {
  private stream: MediaStream | null = null;
  private context: AudioContext | null = null;
  private source: MediaStreamAudioSourceNode | null = null;
  private worklet: AudioWorkletNode | null = null;
  private silence: GainNode | null = null;
  private stopPromise: Promise<void> | null = null;
  private resolveStop: (() => void) | null = null;
  private rejectStop: ((error: Error) => void) | null = null;
  private stopTimer: ReturnType<typeof setTimeout> | null = null;
  private disposed = false;

  private constructor(private readonly options: MicrophonePCMCaptureOptions) {}

  static async start(options: MicrophonePCMCaptureOptions): Promise<MicrophonePCMCapture> {
    const unavailable = realtimeCaptureUnavailableReason();
    if (unavailable) throw new Error(unavailable);

    const capture = new MicrophonePCMCapture(options);
    try {
      await capture.initialize();
      return capture;
    } catch (error) {
      capture.cleanup();
      if (error instanceof DOMException && error.name === "NotAllowedError") {
        throw new Error("麦克风权限被拒绝，请在浏览器设置中允许后重试");
      }
      throw error instanceof Error ? error : new Error("无法启动实时录音");
    }
  }

  async stop(): Promise<void> {
    if (this.disposed) return;
    if (this.stopPromise) return this.stopPromise;
    if (!this.worklet) {
      this.cleanup();
      return;
    }

    this.stopPromise = new Promise<void>((resolve, reject) => {
      this.resolveStop = resolve;
      this.rejectStop = reject;
      this.stopTimer = setTimeout(() => {
        reject(new Error("实时音频采集收尾超时"));
      }, STOP_TIMEOUT_MS);
      this.worklet?.port.postMessage({ type: "stop" });
    }).finally(() => this.cleanup());
    return this.stopPromise;
  }

  dispose(): void {
    this.cleanup();
  }

  private async initialize(): Promise<void> {
    if (this.options.signal?.aborted) throw new DOMException("Aborted", "AbortError");
    this.options.signal?.addEventListener("abort", this.handleAbort, { once: true });

    const stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: { ideal: 1 },
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });
    if (this.disposed || this.options.signal?.aborted) {
      stream.getTracks().forEach((track) => track.stop());
      throw new DOMException("Aborted", "AbortError");
    }
    this.stream = stream;

    const AudioContextCtor = getAudioContextConstructor();
    if (!AudioContextCtor) throw new Error("当前浏览器不支持实时音频采集");
    this.context = new AudioContextCtor();
    await this.context.audioWorklet.addModule(pcmCaptureWorkletURL);
    if (this.disposed || this.options.signal?.aborted) {
      throw new DOMException("Aborted", "AbortError");
    }

    this.source = this.context.createMediaStreamSource(this.stream);
    this.worklet = new AudioWorkletNode(this.context, "pcm-capture", {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      outputChannelCount: [1],
    });
    this.silence = this.context.createGain();
    this.silence.gain.value = 0;

    this.worklet.port.addEventListener("message", this.handleMessage);
    this.worklet.port.addEventListener("messageerror", this.handleMessageError);
    this.worklet.addEventListener("processorerror", this.handleProcessorError);
    for (const track of this.stream.getTracks()) {
      track.addEventListener("ended", this.handleTrackEnded);
    }

    this.source.connect(this.worklet);
    this.worklet.connect(this.silence);
    this.silence.connect(this.context.destination);
    this.worklet.port.start();
    await this.context.resume();
  }

  private readonly handleMessage = (event: MessageEvent<WorkletMessage>): void => {
    if (event.data?.type === "frame") {
      if (event.data.buffer.byteLength > 0 && event.data.buffer.byteLength % 2 === 0) {
        this.options.onFrame(event.data.buffer);
      }
      return;
    }
    if (event.data?.type === "stopped") this.resolveStop?.();
  };

  private readonly handleMessageError = (): void => {
    this.fail(new Error("实时音频数据无法读取"));
  };

  private readonly handleProcessorError = (): void => {
    this.fail(new Error("实时音频处理已中断"));
  };

  private readonly handleTrackEnded = (): void => {
    this.fail(new Error("麦克风采集已结束"));
  };

  private readonly handleAbort = (): void => {
    this.cleanup();
  };

  private fail(error: Error): void {
    if (this.disposed) return;
    this.rejectStop?.(error);
    this.options.onError?.(error);
    this.cleanup();
  }

  private cleanup(): void {
    if (this.disposed) return;
    this.disposed = true;

    if (this.stopTimer !== null) clearTimeout(this.stopTimer);
    this.stopTimer = null;
    this.rejectStop?.(new DOMException("Aborted", "AbortError"));
    this.resolveStop = null;
    this.rejectStop = null;
    this.options.signal?.removeEventListener("abort", this.handleAbort);

    if (this.worklet) {
      this.worklet.port.removeEventListener("message", this.handleMessage);
      this.worklet.port.removeEventListener("messageerror", this.handleMessageError);
      this.worklet.removeEventListener("processorerror", this.handleProcessorError);
      this.worklet.port.close();
      this.worklet.disconnect();
    }
    this.source?.disconnect();
    this.silence?.disconnect();

    for (const track of this.stream?.getTracks() ?? []) {
      track.removeEventListener("ended", this.handleTrackEnded);
      track.stop();
    }
    if (this.context) void this.context.close();

    this.worklet = null;
    this.source = null;
    this.silence = null;
    this.stream = null;
    this.context = null;
  }
}

function getAudioContextConstructor(): AudioContextConstructor | null {
  if (typeof window === "undefined") return null;
  const legacy = window as unknown as LegacyAudioWindow;
  return window.AudioContext ?? legacy.webkitAudioContext ?? null;
}
