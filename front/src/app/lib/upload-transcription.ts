import type { VoiceCapture } from "../api/voice.ts";
import { encodePCM16LEWav } from "./pcm-capture.ts";

export const MAX_UPLOAD_RECORDING_MS = 180000;

export type UploadTranscriptionStatus =
  | "idle"
  | "requesting"
  | "recording"
  | "uploading"
  | "review"
  | "failed";

export interface UploadCaptureOptions {
  onFrame: (pcm16LE: ArrayBuffer) => void;
  onError?: (error: Error) => void;
}

export interface UploadCapture {
  stop(): Promise<void>;
  dispose(): void;
}

export interface UploadTranscriptionDependencies {
  startCapture: (options: UploadCaptureOptions) => Promise<UploadCapture>;
  upload: (sessionID: string, audio: Blob, durationMs: number) => Promise<VoiceCapture>;
  now?: () => number;
}

export interface UploadTranscriptionSnapshot {
  status: UploadTranscriptionStatus;
  text: string;
  voiceCaptureID: string | null;
  capture: VoiceCapture | null;
  error: string;
  elapsedMs: number;
}

const initialSnapshot: UploadTranscriptionSnapshot = {
  status: "idle",
  text: "",
  voiceCaptureID: null,
  capture: null,
  error: "",
  elapsedMs: 0,
};

export class UploadTranscriptionController {
  private capture: UploadCapture | null = null;
  private pcmChunks: Uint8Array[] = [];
  private sessionID: string | null = null;
  private startedAt = 0;
  private generation = 0;
  private listeners = new Set<(snapshot: UploadTranscriptionSnapshot) => void>();
  private current: UploadTranscriptionSnapshot = initialSnapshot;
  private readonly now: () => number;
  private readonly dependencies: UploadTranscriptionDependencies;

  constructor(dependencies: UploadTranscriptionDependencies) {
    this.dependencies = dependencies;
    this.now = dependencies.now ?? Date.now;
  }

  get snapshot(): UploadTranscriptionSnapshot {
    return this.current;
  }

  subscribe = (listener: (snapshot: UploadTranscriptionSnapshot) => void): (() => void) => {
    this.listeners.add(listener);
    listener(this.current);
    return () => this.listeners.delete(listener);
  };

  start = (sessionID: string, prefix = ""): void => {
    if (this.current.status === "requesting" || this.current.status === "recording" || this.current.status === "uploading") {
      return;
    }

    this.releaseCapture();
    const generation = ++this.generation;
    this.pcmChunks = [];
    this.sessionID = sessionID;
    this.startedAt = 0;
    this.update({
      status: "requesting",
      text: prefix,
      voiceCaptureID: null,
      capture: null,
      error: "",
      elapsedMs: 0,
    });

    void this.dependencies.startCapture({
      onFrame: (buffer) => {
        if (generation !== this.generation || buffer.byteLength === 0) return;
        this.pcmChunks.push(new Uint8Array(buffer));
      },
      onError: (error) => this.fail(error, generation),
    }).then((capture) => {
      if (generation !== this.generation) {
        capture.dispose();
        return;
      }
      this.capture = capture;
      this.startedAt = this.now();
      this.update({ status: "recording", elapsedMs: 0 });
    }).catch((error: unknown) => {
      this.fail(error instanceof Error ? error : new Error("无法启动录音"), generation);
    });
  };

  stop = async (): Promise<void> => {
    if (this.current.status !== "recording" || !this.capture || !this.sessionID) return;

    const generation = this.generation;
    const sessionID = this.sessionID;
    const capture = this.capture;
    this.capture = null;
    try {
      await capture.stop();
      if (generation !== this.generation) return;
      const durationMs = Math.min(
        MAX_UPLOAD_RECORDING_MS,
        Math.max(1, Math.round(this.now() - this.startedAt)),
      );
      const wav = encodePCM16LEWav(this.pcmChunks);
      this.update({ status: "uploading", elapsedMs: durationMs });
      const result = await this.dependencies.upload(sessionID, wav, durationMs);
      if (generation !== this.generation) return;
      if (result.status === "failed" || !result.transcript.trim()) {
        this.update({
          status: "failed",
          voiceCaptureID: null,
          capture: result,
          error: result.transcript_error || "没能听清这段话，可以重录或直接打字",
        });
        return;
      }
      this.update({
        status: "review",
        text: this.current.text + result.transcript,
        voiceCaptureID: result.capture_id,
        capture: result,
        error: "",
      });
    } catch (error) {
      this.fail(error instanceof Error ? error : new Error("语音转写失败"), generation);
    } finally {
      capture.dispose();
      if (generation === this.generation) {
        this.pcmChunks = [];
        this.sessionID = null;
      }
    }
  };

  tick = (): void => {
    if (this.current.status !== "recording") return;
    this.update({ elapsedMs: Math.max(0, this.now() - this.startedAt) });
  };

  reset = (text = ""): void => {
    this.generation += 1;
    this.releaseCapture();
    this.pcmChunks = [];
    this.sessionID = null;
    this.update({ ...initialSnapshot, text });
  };

  dispose = (): void => {
    this.generation += 1;
    this.releaseCapture();
    this.pcmChunks = [];
    this.sessionID = null;
    this.listeners.clear();
  };

  private fail(error: Error, generation: number): void {
    if (generation !== this.generation) return;
    this.releaseCapture();
    this.pcmChunks = [];
    this.sessionID = null;
    this.update({
      status: "failed",
      voiceCaptureID: null,
      capture: null,
      error: error.message || "语音转写失败",
    });
  }

  private releaseCapture(): void {
    this.capture?.dispose();
    this.capture = null;
  }

  private update(patch: Partial<UploadTranscriptionSnapshot>): void {
    this.current = { ...this.current, ...patch };
    for (const listener of this.listeners) listener(this.current);
  }
}
