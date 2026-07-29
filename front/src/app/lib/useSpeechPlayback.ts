import { useCallback, useEffect, useRef, useState } from "react";

import { synthesizeSpeech } from "../api/speech";

export type SpeechStatus = "idle" | "loading" | "playing";

// 同一时刻只允许一句话在念。两段音频叠着放谁都听不清，
// 而且模拟面试的场景里“上一问还没念完就冒出下一问”比不出声更糟。
let stopActivePlayback: (() => void) | null = null;

/**
 * useSpeechPlayback 管理单条消息的朗读：合成、播放、打断、资源回收。
 *
 * 朗读失败一律降级为「看文字」，不抛给上层，也不阻断任何操作——
 * 声音只是同一段文字的另一种呈现方式，念不出来不该影响练习本身。
 */
export function useSpeechPlayback() {
  const [status, setStatus] = useState<SpeechStatus>("idle");
  const [error, setError] = useState("");

  const audioRef = useRef<HTMLAudioElement | null>(null);
  const objectURLRef = useRef<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const release = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;

    const audio = audioRef.current;
    if (audio) {
      audio.pause();
      audio.onended = null;
      audio.onerror = null;
      audioRef.current = null;
    }
    // 对象 URL 必须显式释放：每念一句漏一份音频，聊久了内存会一直涨。
    if (objectURLRef.current) {
      URL.revokeObjectURL(objectURLRef.current);
      objectURLRef.current = null;
    }
  }, []);

  const stop = useCallback(() => {
    release();
    setStatus("idle");
  }, [release]);

  useEffect(() => {
    return () => {
      if (stopActivePlayback === stop) stopActivePlayback = null;
      release();
    };
  }, [release, stop]);

  const play = useCallback(
    async (text: string) => {
      stopActivePlayback?.();
      stopActivePlayback = stop;

      setError("");
      setStatus("loading");

      const controller = new AbortController();
      abortRef.current = controller;

      try {
        const url = await synthesizeSpeech(text, controller.signal);
        // 合成期间用户可能已经点了停止或切走，这时候不能再放出声音。
        if (controller.signal.aborted) {
          URL.revokeObjectURL(url);
          return;
        }
        objectURLRef.current = url;

        const audio = new Audio(url);
        audioRef.current = audio;
        audio.onended = () => {
          release();
          setStatus("idle");
        };
        audio.onerror = () => {
          release();
          setStatus("idle");
          setError("音频播放失败");
        };
        await audio.play();
        setStatus("playing");
      } catch (playError) {
        if (controller.signal.aborted) return;
        release();
        setStatus("idle");
        setError(playError instanceof Error ? playError.message : "语音合成失败");
      }
    },
    [release, stop],
  );

  const toggle = useCallback(
    (text: string) => {
      if (status === "idle") {
        void play(text);
        return;
      }
      stop();
    },
    [play, status, stop],
  );

  return { status, error, toggle, stop };
}
