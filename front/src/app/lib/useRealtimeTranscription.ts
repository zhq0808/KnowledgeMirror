import { useEffect, useRef, useState } from "react";

import { realtimeVoiceWebSocketURL } from "../api/realtimeVoice";
import { MicrophonePCMCapture } from "./microphone-pcm-capture";
import {
  RealtimeTranscriptionController,
  type RealtimeTranscriptionDependencies,
  type RealtimeVoiceSocket,
} from "./realtime-transcription";

export interface UseRealtimeTranscriptionOptions {
  dependencies?: RealtimeTranscriptionDependencies;
}

const browserDependencies: RealtimeTranscriptionDependencies = {
  createURL: realtimeVoiceWebSocketURL,
  createSocket: (url) => new WebSocket(url) as unknown as RealtimeVoiceSocket,
  startCapture: (options) => MicrophonePCMCapture.start(options),
};

export function useRealtimeTranscription(options: UseRealtimeTranscriptionOptions = {}) {
  const controllerRef = useRef<RealtimeTranscriptionController | null>(null);
  if (!controllerRef.current) {
    controllerRef.current = new RealtimeTranscriptionController(
      options.dependencies ?? browserDependencies,
    );
  }
  const controller = controllerRef.current;
  const [snapshot, setSnapshot] = useState(controller.snapshot);

  useEffect(() => {
    const unsubscribe = controller.subscribe(setSnapshot);
    return () => {
      unsubscribe();
      controller.dispose();
    };
  }, [controller]);

  return {
    ...snapshot,
    start: controller.start,
    stop: controller.stop,
    reset: controller.reset,
  };
}

export type {
  RealtimeCapture,
  RealtimeCaptureOptions,
  RealtimeTranscriptionDependencies,
  RealtimeTranscriptionSnapshot,
  RealtimeTranscriptionStatus,
  RealtimeVoiceSocket,
} from "./realtime-transcription";