import { useEffect, useRef, useState } from "react";

import { uploadVoiceCapture } from "../api/voice";
import { MicrophonePCMCapture } from "./microphone-pcm-capture";
import {
  UploadTranscriptionController,
  type UploadTranscriptionDependencies,
} from "./upload-transcription";

const browserDependencies: UploadTranscriptionDependencies = {
  startCapture: (options) => MicrophonePCMCapture.start(options),
  upload: uploadVoiceCapture,
};

export interface UseUploadTranscriptionOptions {
  dependencies?: UploadTranscriptionDependencies;
}

export function useUploadTranscription(options: UseUploadTranscriptionOptions = {}) {
  const controllerRef = useRef<UploadTranscriptionController | null>(null);
  if (!controllerRef.current) {
    controllerRef.current = new UploadTranscriptionController(
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

  useEffect(() => {
    if (snapshot.status !== "recording") return;
    const timer = window.setInterval(controller.tick, 200);
    return () => window.clearInterval(timer);
  }, [controller, snapshot.status]);

  return {
    ...snapshot,
    start: controller.start,
    stop: controller.stop,
    reset: controller.reset,
  };
}

export type {
  UploadCapture,
  UploadCaptureOptions,
  UploadTranscriptionDependencies,
  UploadTranscriptionSnapshot,
  UploadTranscriptionStatus,
} from "./upload-transcription";
