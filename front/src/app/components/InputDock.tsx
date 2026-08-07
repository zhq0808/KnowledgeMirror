import { useEffect, useRef, useState } from "react";
import { motion, AnimatePresence } from "motion/react";
import { Mic, RefreshCw, Send, Square, X } from "lucide-react";
import { voiceConfirmationHint } from "../api/voice";
import { useRealtimeTranscription } from "../lib/useRealtimeTranscription";
import { useUploadTranscription } from "../lib/useUploadTranscription";
import { MAX_UPLOAD_RECORDING_MS } from "../lib/upload-transcription";
import { selectVoiceInputMode } from "../lib/voice-mode";

const PROMPTS = [
  { emoji: "🧠", label: "费曼学习" },
  { emoji: "🔄", label: "知识点回顾" },
  { emoji: "💬", label: "模拟面试" },
  { emoji: "🎯", label: "JD 分析" },
];

interface InputDockProps {
  // onSendMessage 的 voiceCaptureID 只在这条消息来自语音时带上，
  // 用于把原始转写和用户最终发出的文本关联起来。
  onSendMessage: (message: string, voiceCaptureID?: string) => void;
  sessionID: string | null;
  onSelectPrompt: (prompt: { emoji: string; label: string }) => void;
  realtimeVoiceEnabled?: boolean;
  fileVoiceEnabled?: boolean;
  voiceCapabilityLoading?: boolean;
  voiceCapabilityError?: string | null;
  onRetryVoiceCapabilities?: () => void;
  isResponding: boolean;
  disabled?: boolean;
  onStop: () => void;
  onHeightChange?: (height: number) => void;
}

export function InputDock({
  onSendMessage,
  sessionID,
  onSelectPrompt,
  realtimeVoiceEnabled = false,
  fileVoiceEnabled = false,
  voiceCapabilityLoading = false,
  voiceCapabilityError = null,
  onRetryVoiceCapabilities,
  isResponding,
  disabled = false,
  onStop,
  onHeightChange,
}: InputDockProps) {
  const [input, setInput] = useState("");
  const [voiceNotice, setVoiceNotice] = useState("");
  const [preferUploadFallback, setPreferUploadFallback] = useState(false);
  const textInputRef = useRef<HTMLInputElement>(null);
  const dockRef = useRef<HTMLDivElement>(null);
  const realtime = useRealtimeTranscription();
  const upload = useUploadTranscription();
  const preferredVoiceMode = selectVoiceInputMode(realtimeVoiceEnabled, fileVoiceEnabled);
  const voiceMode =
    preferUploadFallback && fileVoiceEnabled ? "upload" : preferredVoiceMode;

  const realtimeBusy =
    realtime.status === "connecting" ||
    realtime.status === "ready" ||
    realtime.status === "streaming" ||
    realtime.status === "stopping";
  const uploadBusy =
    upload.status === "requesting" ||
    upload.status === "recording" ||
    upload.status === "uploading";
  const voiceBusy = realtimeBusy || uploadBusy;

  useEffect(() => {
    if (realtime.status !== "idle") setInput(realtime.text);
  }, [realtime.status, realtime.text]);

  useEffect(() => {
    if (upload.status !== "idle") setInput(upload.text);
  }, [upload.status, upload.text]);

  useEffect(() => {
    realtime.reset("");
    upload.reset("");
    setInput("");
    setVoiceNotice("");
    setPreferUploadFallback(false);
  }, [sessionID, realtime.reset, upload.reset]);

  useEffect(() => {
    if (upload.status === "recording" && upload.elapsedMs >= MAX_UPLOAD_RECORDING_MS) {
      void upload.stop();
    }
  }, [upload.elapsedMs, upload.status, upload.stop]);

  useEffect(() => {
    const dock = dockRef.current;
    if (!dock || !onHeightChange) return;

    const reportHeight = () => onHeightChange(Math.ceil(dock.getBoundingClientRect().height));
    reportHeight();
    const observer = new ResizeObserver(reportHeight);
    observer.observe(dock);
    return () => observer.disconnect();
  }, [onHeightChange]);

  const resetVoice = (text: string) => {
    realtime.reset(text);
    upload.reset(text);
  };

  const submitText = (text: string) => {
    const voiceCaptureID = realtime.voiceCaptureID ?? upload.voiceCaptureID ?? undefined;
    setInput("");
    setVoiceNotice("");
    resetVoice("");
    onSendMessage(text, voiceCaptureID);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!disabled && !voiceBusy && input.trim()) submitText(input.trim());
  };

  const stopVoice = () => {
    if (realtime.status === "connecting" || realtime.status === "ready" || realtime.status === "streaming") {
      void realtime.stop();
      return;
    }
    if (upload.status === "recording") void upload.stop();
  };

  const startVoice = () => {
    if (disabled || voiceBusy) return;
    if (voiceCapabilityLoading) {
      setVoiceNotice("正在确认语音服务，请稍候后重试");
      return;
    }
    if (voiceCapabilityError) {
      setVoiceNotice(`${voiceCapabilityError}。请检查网络后重试`);
      return;
    }
    if (!sessionID) {
      setVoiceNotice("会话尚未准备好，请稍候后重试");
      return;
    }

    setVoiceNotice("");
    if (voiceMode === "realtime") {
      realtime.start(sessionID, input);
      return;
    }
    if (voiceMode === "upload") {
      upload.start(sessionID, input);
      return;
    }
    setVoiceNotice("当前服务未配置语音输入，请使用文字输入或联系管理员启用语音服务");
  };

  const stopResponding = () => {
    resetVoice(input);
    onStop();
  };

  const canStopRealtime =
    realtime.status === "connecting" ||
    realtime.status === "ready" ||
    realtime.status === "streaming";
  const canStopUpload = upload.status === "recording";
  const canStopVoice = canStopRealtime || canStopUpload;
  const micDisabled = disabled || realtime.status === "stopping" || upload.status === "requesting" || upload.status === "uploading";
  const micTitle = canStopVoice
    ? voiceMode === "realtime"
      ? "停止听写"
      : "停止录音并转写"
    : voiceCapabilityLoading
      ? "点击查看语音服务状态"
      : voiceCapabilityError
        ? "语音服务状态加载失败，点击查看重试方式"
        : voiceMode === "realtime"
          ? "实时语音输入"
          : fileVoiceEnabled
            ? "录音后转写"
            : "语音输入暂不可用，点击查看原因";

  const capabilityStatus = voiceCapabilityError
    ? { text: `${voiceCapabilityError}。请检查网络后重试`, failed: true, active: false }
    : !voiceCapabilityLoading && preferredVoiceMode === null
      ? {
          text: "当前服务未配置语音输入，请使用文字输入或联系管理员启用语音服务",
          failed: true,
          active: false,
        }
      : null;

  const activeVoiceStatus = (() => {
    switch (realtime.status) {
      case "connecting":
        return { text: "正在连接实时转写…", failed: false, active: true };
      case "ready":
        return { text: "麦克风已就绪，正在开始听写…", failed: false, active: true };
      case "streaming":
        return { text: "正在听写，文字会实时显示", failed: false, active: true };
      case "stopping":
        return { text: "正在收尾，请稍候…", failed: false, active: true };
      case "review":
        return { text: "转写完成，可修改后发送", failed: false, active: false };
      case "failed":
        return { text: realtime.error || "实时转写失败，可保留当前文字或重新听写", failed: true, active: false };
    }

    switch (upload.status) {
      case "requesting":
        return { text: "正在申请麦克风权限…", failed: false, active: true };
      case "recording":
        return { text: `正在录音 ${(upload.elapsedMs / 1000).toFixed(1)}s`, failed: false, active: true };
      case "uploading":
        return { text: "录音完成，正在上传转写…", failed: false, active: true };
      case "review":
        return {
          text: upload.capture ? voiceConfirmationHint(upload.capture) || "转写完成，可修改后发送" : "转写完成，可修改后发送",
          failed: false,
          active: false,
        };
      case "failed":
        return { text: upload.error || "语音转写失败，可重新录音或使用文字输入", failed: true, active: false };
      default:
        return voiceNotice
          ? { text: voiceNotice, failed: true, active: false }
          : capabilityStatus;
    }
  })();

  const clearVoiceStatus = () => {
    setVoiceNotice("");
    resetVoice(input);
  };

  const canDismissVoiceStatus =
    Boolean(voiceNotice) ||
    realtime.status === "failed" ||
    upload.status === "failed";

  const switchToUploadFallback = () => {
    realtime.reset(input);
    setPreferUploadFallback(true);
    setVoiceNotice("实时转写暂不可用，已切换为录音后转写，请再次点击麦克风");
  };

  return (
    <div ref={dockRef} className="absolute bottom-[80px] left-0 right-0 z-30 bg-gradient-to-t from-[#F6F8F4] via-[#F6F8F4]/96 to-transparent px-5 pb-4 pt-8">
      <div className="mb-3 flex items-center gap-2 overflow-x-auto no-scrollbar">
        {PROMPTS.map((prompt) => (
          <button
            key={prompt.label}
            type="button"
            onClick={() => onSelectPrompt(prompt)}
            disabled={disabled}
            className="flex-shrink-0 flex items-center gap-1.5 rounded-full bg-white px-3.5 py-2 text-xs text-gray-600 shadow-[0_2px_12px_rgba(0,0,0,0.05)] transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            <span>{prompt.emoji}</span>
            <span>{prompt.label}</span>
          </button>
        ))}
      </div>

      <AnimatePresence>
        {activeVoiceStatus && (
          <motion.div
            initial={{ opacity: 0, y: 6 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 6 }}
            transition={{ duration: 0.15 }}
            className="mb-2 rounded-2xl bg-white px-4 py-2.5 shadow-[0_2px_12px_rgba(0,0,0,0.05)]"
            role="status"
          >
            <div className={`flex min-h-4 items-center gap-2 text-xs ${activeVoiceStatus.failed ? "text-[#C0603A]" : "text-gray-600"}`}>
              {activeVoiceStatus.active && (
                <span className="h-2 w-2 flex-shrink-0 animate-pulse rounded-full bg-[#F4A460]" />
              )}
              <span className="min-w-0 flex-1">{activeVoiceStatus.text}</span>
              {(realtime.status === "streaming" || upload.status === "recording") && (
                <span className="flex-shrink-0 text-gray-400">再次点击麦克风结束</span>
              )}
              {voiceCapabilityError && onRetryVoiceCapabilities && !voiceBusy && (
                <button
                  type="button"
                  onClick={onRetryVoiceCapabilities}
                  className="flex flex-shrink-0 items-center gap-1 font-medium text-[#2E5E3E] hover:underline"
                >
                  <RefreshCw size={12} />
                  重试
                </button>
              )}
              {realtime.status === "failed" && fileVoiceEnabled && !preferUploadFallback && (
                <button
                  type="button"
                  onClick={switchToUploadFallback}
                  className="flex-shrink-0 font-medium text-[#2E5E3E] hover:underline"
                >
                  改用录音转写
                </button>
              )}
              {activeVoiceStatus.failed && canDismissVoiceStatus && (
                <button
                  type="button"
                  onClick={clearVoiceStatus}
                  aria-label="关闭提示"
                  className="flex-shrink-0 text-gray-400 hover:text-gray-600"
                >
                  <X size={13} />
                </button>
              )}
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      <form onSubmit={handleSubmit} className="flex items-center gap-2.5">
        <div className="flex-1 relative">
          <input
            ref={textInputRef}
            type="text"
            value={input}
            onChange={(event) => setInput(event.target.value)}
            readOnly={voiceBusy || disabled}
            aria-readonly={voiceBusy || disabled}
            placeholder="输入想练习的知识点或面试问题..."
            className="w-full bg-white rounded-full px-5 py-3.5 pr-12 shadow-[0_2px_20px_rgba(0,0,0,0.06)] focus:outline-none focus:ring-2 focus:ring-[#A8D5BA]/30 transition-shadow text-sm read-only:cursor-default"
          />
          <AnimatePresence>
            {input && !voiceBusy && !disabled && (
              <motion.button
                type="submit"
                initial={{ opacity: 0, scale: 0.7 }}
                animate={{ opacity: 1, scale: 1 }}
                exit={{ opacity: 0, scale: 0.7 }}
                transition={{ duration: 0.15 }}
                className="absolute right-2 top-1/2 -translate-y-1/2 w-8 h-8 rounded-full bg-[#A8D5BA] text-white flex items-center justify-center hover:bg-[#95C4A8] transition-colors"
              >
                <Send className="w-3.5 h-3.5" />
              </motion.button>
            )}
          </AnimatePresence>
        </div>

        {isResponding ? (
          <motion.button
            type="button"
            onClick={stopResponding}
            aria-label="停止回答"
            title="停止回答"
            initial={{ scale: 0.8, opacity: 0 }}
            animate={{ scale: 1, opacity: 1 }}
            className="w-11 h-11 rounded-full flex items-center justify-center shadow-[0_2px_16px_rgba(0,0,0,0.07)] bg-primary text-white transition-colors flex-shrink-0"
          >
            <Square className="w-4 h-4" fill="currentColor" />
          </motion.button>
        ) : (
          <motion.button
            type="button"
            onClick={() => {
              if (canStopVoice) stopVoice();
              else startVoice();
            }}
            disabled={micDisabled}
            aria-label={canStopVoice ? "停止语音输入" : "语音输入"}
            title={micTitle}
            className={`w-11 h-11 rounded-full flex items-center justify-center shadow-[0_2px_16px_rgba(0,0,0,0.07)] transition-colors flex-shrink-0 ${
              canStopVoice
                ? "bg-[#F4A460] text-white"
                : "bg-white text-gray-500 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-40"
            }`}
          >
            {canStopVoice ? <Square className="h-4 w-4" fill="currentColor" /> : <Mic className="w-[18px] h-[18px]" />}
          </motion.button>
        )}
      </form>
    </div>
  );
}
