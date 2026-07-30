import {
  parseRealtimeVoiceEvent,
  type RealtimeVoiceTranscriptEvent,
} from "../api/realtimeVoice.ts";

const WEB_SOCKET_OPEN = 1;

export type RealtimeTranscriptionStatus =
  | "idle"
  | "connecting"
  | "ready"
  | "streaming"
  | "stopping"
  | "review"
  | "failed";

export interface RealtimeTranscriptionSnapshot {
  status: RealtimeTranscriptionStatus;
  text: string;
  prefix: string;
  committedSentences: readonly string[];
  interim: string;
  voiceCaptureID: string | null;
  error: string;
  retryable: boolean;
}

export interface RealtimeCaptureOptions {
  onFrame: (pcm16LE: ArrayBuffer) => void;
  onError: (error: Error) => void;
  signal: AbortSignal;
}

export interface RealtimeCapture {
  stop(): Promise<void>;
  dispose(): void;
}

export interface RealtimeVoiceSocket {
  binaryType: BinaryType;
  readonly readyState: number;
  onmessage: ((event: { data: unknown }) => void) | null;
  onerror: (() => void) | null;
  onclose: (() => void) | null;
  send(data: string | ArrayBuffer): void;
  close(code?: number, reason?: string): void;
}

export interface RealtimeTranscriptionDependencies {
  createURL: (sessionID: string) => string;
  createSocket: (url: string) => RealtimeVoiceSocket;
  startCapture: (options: RealtimeCaptureOptions) => Promise<RealtimeCapture>;
}

type SnapshotListener = (snapshot: RealtimeTranscriptionSnapshot) => void;

const initialSnapshot: RealtimeTranscriptionSnapshot = {
  status: "idle",
  text: "",
  prefix: "",
  committedSentences: [],
  interim: "",
  voiceCaptureID: null,
  error: "",
  retryable: false,
};

export class RealtimeTranscriptionController {
  private readonly dependencies: RealtimeTranscriptionDependencies;
  private currentSnapshot = initialSnapshot;
  private readonly listeners = new Set<SnapshotListener>();
  private readonly committed = new Map<number, string>();
  private interim: { sentenceID: number; text: string } | null = null;
  private socket: RealtimeVoiceSocket | null = null;
  private capture: RealtimeCapture | null = null;
  private capturePromise: Promise<RealtimeCapture> | null = null;
  private captureAbort: AbortController | null = null;
  private stopPromise: Promise<void> | null = null;
  private operation = 0;
  private lastSeq = 0;
  private stopSent = false;
  private terminal = false;

  constructor(dependencies: RealtimeTranscriptionDependencies) {
    this.dependencies = dependencies;
  }

  get snapshot(): RealtimeTranscriptionSnapshot {
    return this.currentSnapshot;
  }

  subscribe = (listener: SnapshotListener): (() => void) => {
    this.listeners.add(listener);
    listener(this.currentSnapshot);
    return () => this.listeners.delete(listener);
  };

  start = (sessionID: string, prefix: string): void => {
    this.releaseResources();
    this.committed.clear();
    this.interim = null;
    this.lastSeq = 0;
    this.stopSent = false;
    this.terminal = false;

    const operation = this.operation;
    this.publish({
      status: "connecting",
      text: prefix,
      prefix,
      committedSentences: [],
      interim: "",
      voiceCaptureID: null,
      error: "",
      retryable: false,
    });

    try {
      const socket = this.dependencies.createSocket(this.dependencies.createURL(sessionID));
      socket.binaryType = "arraybuffer";
      socket.onmessage = (event) => this.handleSocketMessage(operation, event.data);
      socket.onerror = () => this.fail(operation, "无法连接实时转写服务", true);
      socket.onclose = () => {
        if (!this.terminal) this.fail(operation, "实时转写连接已断开", true);
      };
      this.socket = socket;
    } catch (error) {
      this.fail(operation, errorMessage(error, "无法连接实时转写服务"), true);
    }
  };

  stop = (): Promise<void> => {
    if (this.currentSnapshot.status === "connecting") {
      const prefix = this.currentSnapshot.prefix;
      this.releaseResources();
      this.publish({ ...initialSnapshot, text: prefix, prefix });
      return Promise.resolve();
    }
    if (this.currentSnapshot.status !== "ready" && this.currentSnapshot.status !== "streaming") {
      return this.stopPromise ?? Promise.resolve();
    }
    if (this.stopPromise) return this.stopPromise;

    const operation = this.operation;
    this.publish({ ...this.currentSnapshot, status: "stopping" });
    this.stopPromise = this.finishCaptureAndRequestCompletion(operation).finally(() => {
      if (operation === this.operation) this.stopPromise = null;
    });
    return this.stopPromise;
  };

  reset = (text = ""): void => {
    this.releaseResources();
    this.committed.clear();
    this.interim = null;
    this.lastSeq = 0;
    this.publish({ ...initialSnapshot, text, prefix: text });
  };

  dispose = (): void => {
    this.releaseResources();
    this.listeners.clear();
  };

  private handleSocketMessage(operation: number, raw: unknown): void {
    if (operation !== this.operation || this.terminal) return;

    try {
      const event = parseRealtimeVoiceEvent(raw);
      switch (event.type) {
        case "ready":
          this.handleReady(operation);
          return;
        case "transcript":
          this.mergeTranscript(event);
          return;
        case "completed": {
          const prefix = this.currentSnapshot.prefix;
          this.terminal = true;
          this.releaseResources();
          this.publish({
            status: "review",
            text: prefix + event.capture.transcript,
            prefix,
            committedSentences: event.capture.transcript ? [event.capture.transcript] : [],
            interim: "",
            voiceCaptureID: event.capture.capture_id,
            error: "",
            retryable: false,
          });
          return;
        }
        case "error":
          this.fail(operation, event.message || "实时转写失败", event.retryable);
      }
    } catch (error) {
      this.fail(operation, errorMessage(error, "实时转写消息处理失败"), true);
    }
  }

  private handleReady(operation: number): void {
    if (this.currentSnapshot.status !== "connecting") return;

    this.publish({ ...this.currentSnapshot, status: "ready" });
    const captureAbort = new AbortController();
    this.captureAbort = captureAbort;
    const capturePromise = this.dependencies.startCapture({
      signal: captureAbort.signal,
      onFrame: (frame) => this.sendAudioFrame(operation, frame),
      onError: (error) => this.fail(operation, error.message || "实时音频采集失败", false),
    });
    this.capturePromise = capturePromise;

    void capturePromise
      .then((capture) => {
        if (operation !== this.operation || this.terminal) {
          capture.dispose();
          return;
        }
        this.capture = capture;
        if (this.currentSnapshot.status === "ready") {
          this.publish({ ...this.currentSnapshot, status: "streaming" });
        }
      })
      .catch((error) => {
        if (operation !== this.operation || captureAbort.signal.aborted) return;
        this.fail(operation, errorMessage(error, "无法启动实时音频采集"), false);
      });
  }

  private sendAudioFrame(operation: number, frame: ArrayBuffer): void {
    if (
      operation !== this.operation ||
      this.stopSent ||
      (this.currentSnapshot.status !== "streaming" && this.currentSnapshot.status !== "stopping")
    ) {
      return;
    }
    if (!this.socket || this.socket.readyState !== WEB_SOCKET_OPEN) {
      this.fail(operation, "实时转写连接已断开", true);
      return;
    }
    try {
      this.socket.send(frame);
    } catch {
      this.fail(operation, "音频发送失败", true);
    }
  }

  private async finishCaptureAndRequestCompletion(operation: number): Promise<void> {
    try {
      if (this.capturePromise) await this.capturePromise;
      if (operation !== this.operation || this.terminal) return;

      await this.capture?.stop();
      this.capture = null;
      if (operation !== this.operation || this.terminal) return;
      if (!this.socket || this.socket.readyState !== WEB_SOCKET_OPEN) {
        this.fail(operation, "实时转写连接已断开", true);
        return;
      }

      this.stopSent = true;
      this.socket.send(JSON.stringify({ type: "stop" }));
    } catch (error) {
      if (operation !== this.operation || this.terminal) return;
      this.fail(operation, errorMessage(error, "实时音频收尾失败"), false);
    }
  }

  private mergeTranscript(event: RealtimeVoiceTranscriptEvent): void {
    if (
      (this.currentSnapshot.status !== "ready" &&
        this.currentSnapshot.status !== "streaming" &&
        this.currentSnapshot.status !== "stopping") ||
      event.seq <= this.lastSeq
    ) {
      return;
    }
    this.lastSeq = event.seq;
    if (this.committed.has(event.sentence_id)) return;

    if (event.sentence_end) {
      this.committed.set(event.sentence_id, event.text);
      if (this.interim?.sentenceID === event.sentence_id) this.interim = null;
    } else {
      this.interim = { sentenceID: event.sentence_id, text: event.text };
    }
    this.publishTextState();
  }

  private publishTextState(): void {
    const committedSentences = [...this.committed.entries()]
      .sort(([left], [right]) => left - right)
      .map(([, text]) => text);
    const interim = this.interim?.text ?? "";
    this.publish({
      ...this.currentSnapshot,
      text: this.currentSnapshot.prefix + committedSentences.join("") + interim,
      committedSentences,
      interim,
      voiceCaptureID: null,
    });
  }

  private fail(operation: number, message: string, retryable: boolean): void {
    if (operation !== this.operation || this.terminal) return;
    const snapshot = this.currentSnapshot;
    this.terminal = true;
    this.releaseResources();
    this.publish({
      ...snapshot,
      status: "failed",
      voiceCaptureID: null,
      error: message,
      retryable,
    });
  }

  private releaseResources(): void {
    this.operation += 1;
    this.captureAbort?.abort();
    this.captureAbort = null;
    this.capture?.dispose();
    this.capture = null;
    this.capturePromise = null;
    this.stopPromise = null;

    const socket = this.socket;
    this.socket = null;
    if (socket) {
      socket.onmessage = null;
      socket.onerror = null;
      socket.onclose = null;
      socket.close(1000, "client cleanup");
    }
  }

  private publish(snapshot: RealtimeTranscriptionSnapshot): void {
    this.currentSnapshot = snapshot;
    for (const listener of this.listeners) listener(snapshot);
  }
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}