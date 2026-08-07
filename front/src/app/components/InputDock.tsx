import { useEffect, useRef, useState } from "react";
import { motion, AnimatePresence } from "motion/react";
import { Mic, Send, Square, X } from "lucide-react";
import { useRealtimeTranscription } from "../lib/useRealtimeTranscription";

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
  voiceCapabilitiesLoaded?: boolean;
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
  voiceCapabilitiesLoaded = false,
  isResponding,
  disabled = false,
  onStop,
  onHeightChange,
}: InputDockProps) {
  const [input, setInput] = useState("");
  const textInputRef = useRef<HTMLInputElement>(null);
  const dockRef = useRef<HTMLDivElement>(null);
  const realtime = useRealtimeTranscription();
  const voiceBusy =
    realtime.status === "connecting" ||
    realtime.status === "ready" ||
    realtime.status === "streaming" ||
    realtime.status === "stopping";

  useEffect(() => {
    if (realtime.status !== "idle") setInput(realtime.text);
  }, [realtime.status, realtime.text]);

  useEffect(() => {
    realtime.reset("");
    setInput("");
  }, [sessionID, realtime.reset]);

  useEffect(() => {
    const dock = dockRef.current;
    if (!dock || !onHeightChange) return;

    const reportHeight = () => onHeightChange(Math.ceil(dock.getBoundingClientRect().height));
    reportHeight();
    const observer = new ResizeObserver(reportHeight);
    observer.observe(dock);
    return () => observer.disconnect();
  }, [onHeightChange]);

  const submitText = (text: string) => {
    // 只有这次发送确实来自录音时才带 capture_id；
    // 用户把转写改得面目全非也没关系，原始转写在后端原样存着，谁也没覆盖谁。
    const voiceCaptureID = realtime.voiceCaptureID ?? undefined;
    setInput("");
    realtime.reset("");
    onSendMessage(text, voiceCaptureID);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!disabled && !voiceBusy && input.trim()) submitText(input.trim());
  };

  const stopRealtime = () => {
    void realtime.stop();
  };

  const startRealtime = () => {
    if (disabled || !sessionID || !realtimeVoiceEnabled || voiceBusy) return;
    realtime.start(sessionID, input);
  };

  const stopResponding = () => {
    realtime.reset(input);
    onStop();
  };

  const canStopRealtime =
    realtime.status === "connecting" ||
    realtime.status === "ready" ||
    realtime.status === "streaming";
  const micDisabled = disabled || !voiceCapabilitiesLoaded || !realtimeVoiceEnabled || !sessionID || realtime.status === "stopping";
  const micTitle =
    canStopRealtime
      ? "停止听写"
      : realtime.status === "stopping"
        ? "正在收尾"
        : realtime.status === "review"
          ? "重新听写"
          : !voiceCapabilitiesLoaded
            ? "正在确认实时 ASR 服务"
            : !realtimeVoiceEnabled
              ? "实时 ASR 服务未配置"
              : "实时语音输入";

  const voiceStatusText = (() => {
    switch (realtime.status) {
      case "connecting":
        return "正在连接实时转写…";
      case "ready":
        return "麦克风已就绪，正在开始听写…";
      case "streaming":
        return "正在听写，文字会实时显示";
      case "stopping":
        return "正在收尾，请稍候…";
      case "review":
        return "转写完成，可修改后发送";
      case "failed":
        return realtime.error || "实时转写失败，可保留当前文字或重新听写";
      default:
        return "";
    }
  })();

  return (
    <div ref={dockRef} className="absolute bottom-[80px] left-0 right-0 z-30 bg-gradient-to-t from-[#F6F8F4] via-[#F6F8F4]/96 to-transparent px-5 pb-4 pt-8">
      {/* 快捷提示（可横向滚动） */}
      <div className="mb-3 flex items-center gap-2 overflow-x-auto no-scrollbar">
          {PROMPTS.map((p) => (
            <button
              key={p.label}
              type="button"
              onClick={() => onSelectPrompt(p)}
              disabled={disabled}
              className="flex-shrink-0 flex items-center gap-1.5 rounded-full bg-white px-3.5 py-2 text-xs text-gray-600 shadow-[0_2px_12px_rgba(0,0,0,0.05)] transition-colors hover:bg-gray-50 active:bg-gray-100 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <span>{p.emoji}</span>
              <span>{p.label}</span>
            </button>
          ))}
      </div>

      {/* 状态条只描述实时链路；转写正文始终显示在原输入框中。 */}
      {realtime.status !== "idle" && (
        <motion.div
          initial={{ opacity: 0, y: 6 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.15 }}
          className="mb-2 rounded-2xl bg-white px-4 py-2.5 shadow-[0_2px_12px_rgba(0,0,0,0.05)]"
        >
          <div
            className={`flex min-h-4 items-center gap-2 text-xs ${
              realtime.status === "failed" ? "text-[#C0603A]" : "text-gray-600"
            }`}
          >
            {(realtime.status === "connecting" ||
              realtime.status === "ready" ||
              realtime.status === "streaming" ||
              realtime.status === "stopping") && (
              <span className="h-2 w-2 flex-shrink-0 animate-pulse rounded-full bg-[#F4A460]" />
            )}
            <span className="min-w-0 flex-1 truncate">{voiceStatusText}</span>
            {realtime.status === "streaming" && (
              <span className="flex-shrink-0 text-gray-400">点击麦克风结束</span>
            )}
            {realtime.status === "failed" && (
              <button
                type="button"
                onClick={() => {
                  realtime.reset(input);
                }}
                aria-label="关闭提示"
                className="flex-shrink-0 text-gray-400 hover:text-gray-600"
              >
                <X size={13} />
              </button>
            )}
          </div>
        </motion.div>
      )}

      <form onSubmit={handleSubmit} className="flex items-center gap-2.5">
        <div className="flex-1 relative">
          <input
            ref={textInputRef}
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
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
              if (canStopRealtime) {
                stopRealtime();
                return;
              }
              startRealtime();
            }}
            disabled={micDisabled}
            aria-label={canStopRealtime ? "停止听写" : "实时语音输入"}
            title={micTitle}
            className={`w-11 h-11 rounded-full flex items-center justify-center shadow-[0_2px_16px_rgba(0,0,0,0.07)] transition-colors flex-shrink-0 ${
              canStopRealtime
                ? "bg-[#F4A460] text-white"
                : "bg-white text-gray-500 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-40"
            }`}
          >
            {canStopRealtime ? <Square className="h-4 w-4" fill="currentColor" /> : <Mic className="w-[18px] h-[18px]" />}
          </motion.button>
        )}
      </form>
    </div>
  );
}
