import { useEffect, useRef, useState } from "react";
import { motion, AnimatePresence } from "motion/react";
import { Mic, Send, Plus, Square, Sparkles, ChevronDown, Check, X } from "lucide-react";
import type { ModelOption } from "../api/chat";
import { encodeRecordingAsWav, recordingUnavailableReason } from "../lib/audio";
import {
  getVoiceAlwaysConfirm,
  setVoiceAlwaysConfirm,
  voiceConfirmationHint,
  type VoiceCapture,
} from "../api/voice";

const PROMPTS = [
  { emoji: "🧠", label: "费曼学习" },
  { emoji: "🔄", label: "知识点回顾" },
  { emoji: "💬", label: "模拟面试" },
  { emoji: "🎯", label: "JD 分析" },
];

// 录音时长上限与后端 voice.max_duration_ms 保持一致：
// 前端到点自动停，好过让用户讲完一大段才被后端拒掉。
const maxRecordingMs = 180000;

interface InputDockProps {
  // onSendMessage 的 voiceCaptureID 只在这条消息来自语音时带上，
  // 用于把原始转写和用户最终发出的文本关联起来。
  onSendMessage: (message: string, voiceCaptureID?: string) => void;
  // onVoiceCapture 上传一段录音并返回转写结果；为空时语音入口不可用。
  onVoiceCapture?: (audio: Blob, durationMs: number) => Promise<VoiceCapture>;
  onSelectPrompt: (prompt: { emoji: string; label: string }) => void;
  activePrompt?: string;
  voiceDisabled?: boolean;
  // voiceTitle 覆盖麦克风按钮的提示文案，用于说明按钮为什么不可用。
  voiceTitle?: string;
  onPhoto: (file: File) => void;
  isResponding: boolean;
  onStop: () => void;
  models: ModelOption[];
  selectedModelID: string;
  onSelectModel: (modelID: string) => void;
}

export function InputDock({
  onSendMessage,
  onVoiceCapture,
  onSelectPrompt,
  activePrompt,
  voiceDisabled = false,
  voiceTitle,
  onPhoto,
  isResponding,
  onStop,
  models,
  selectedModelID,
  onSelectModel,
}: InputDockProps) {
  const [input, setInput] = useState("");
  const [modelMenuOpen, setModelMenuOpen] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const textInputRef = useRef<HTMLInputElement>(null);

  // 语音状态机：idle -> requesting（申请麦克风）-> recording -> uploading -> idle。
  const [voiceState, setVoiceState] = useState<"idle" | "requesting" | "recording" | "uploading">("idle");
  const [elapsedMs, setElapsedMs] = useState(0);
  const [voiceError, setVoiceError] = useState("");
  // pendingCapture 非空 = 这段转写还没发出去，正等用户看一眼。
  const [pendingCapture, setPendingCapture] = useState<VoiceCapture | null>(null);
  const [alwaysConfirm, setAlwaysConfirm] = useState(false);

  const recorderRef = useRef<MediaRecorder | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const chunksRef = useRef<Blob[]>([]);
  const startedAtRef = useRef(0);
  const timerRef = useRef<number | null>(null);

  useEffect(() => {
    setAlwaysConfirm(getVoiceAlwaysConfirm());
    return () => {
      if (timerRef.current !== null) window.clearInterval(timerRef.current);
      const recorder = recorderRef.current;
      if (recorder && recorder.state !== "inactive") {
        recorder.ondataavailable = null;
        recorder.onstop = null;
        recorder.stop();
      }
      streamRef.current?.getTracks().forEach((track) => track.stop());
    };
  }, []);

  const selectedModel =
    models.find((m) => m.id === selectedModelID) ?? models[0];

  const submitText = (text: string) => {
    // 只有这次发送确实来自录音时才带 capture_id；
    // 用户把转写改得面目全非也没关系，原始转写在后端原样存着，谁也没覆盖谁。
    onSendMessage(text, pendingCapture?.capture_id);
    setInput("");
    setPendingCapture(null);
    setVoiceError("");
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (input.trim()) submitText(input.trim());
  };

  const handleFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) {
      onPhoto(file);
    }
    // 重置，允许连续选择同一文件时也能再次触发 change。
    e.target.value = "";
  };

  const uploadRecording = async (blob: Blob, durationMs: number) => {
    if (!onVoiceCapture) return;
    setVoiceState("uploading");
    try {
      // 浏览器只能录出 webm/opus 这类压缩格式，而 STT 供应商只收 wav/mp3，
      // 所以这里必须先转一道码再上传；失败时抛出的就是给用户看的话。
      const wav = await encodeRecordingAsWav(blob);
      const capture = await onVoiceCapture(wav, durationMs);
      if (capture.status === "failed" || !capture.transcript) {
        // 失败不清空输入框：用户可能已经先打了一半字。
        setVoiceError(voiceConfirmationHint(capture) || "没能听清这段话，可以重录或直接打字");
        setPendingCapture(null);
        return;
      }
      // 默认直接发送：讲完一句还要手动确认一次，语音就比打字还累了。
      // 只有后端拿出明确证据（置信度低／术语可疑），或用户自己选了“每次都先确认”，才停一下。
      if (!capture.needs_confirmation && !alwaysConfirm) {
        setPendingCapture(null);
        setVoiceError("");
        onSendMessage(capture.transcript, capture.capture_id);
        return;
      }
      setInput(capture.transcript);
      setPendingCapture(capture);
      setVoiceError("");
      window.setTimeout(() => textInputRef.current?.focus(), 0);
    } catch (uploadError) {
      setVoiceError(uploadError instanceof Error ? uploadError.message : "语音转写失败");
      setPendingCapture(null);
    } finally {
      setVoiceState("idle");
      setElapsedMs(0);
    }
  };

  const stopRecording = () => {
    const recorder = recorderRef.current;
    if (recorder && recorder.state !== "inactive") recorder.stop();
  };

  const startRecording = async () => {
    if (!onVoiceCapture || voiceDisabled || voiceState !== "idle") return;
    const unavailable = recordingUnavailableReason();
    if (unavailable) {
      setVoiceError(unavailable);
      return;
    }
    setVoiceError("");
    setVoiceState("requesting");
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      streamRef.current = stream;
      const preferred = ["audio/webm;codecs=opus", "audio/mp4", "audio/ogg;codecs=opus"].find((type) =>
        MediaRecorder.isTypeSupported(type),
      );
      const recorder = new MediaRecorder(stream, preferred ? { mimeType: preferred } : undefined);
      recorderRef.current = recorder;
      chunksRef.current = [];
      recorder.ondataavailable = (event) => {
        if (event.data.size > 0) chunksRef.current.push(event.data);
      };
      recorder.onstop = () => {
        const durationMs = Math.max(1, Date.now() - startedAtRef.current);
        if (timerRef.current !== null) window.clearInterval(timerRef.current);
        timerRef.current = null;
        stream.getTracks().forEach((track) => track.stop());
        const blob = new Blob(chunksRef.current, {
          type: recorder.mimeType || chunksRef.current[0]?.type || "audio/webm",
        });
        if (blob.size > 0) {
          void uploadRecording(blob, durationMs);
        } else {
          setVoiceState("idle");
          setElapsedMs(0);
        }
      };
      startedAtRef.current = Date.now();
      setElapsedMs(0);
      recorder.start(250);
      setVoiceState("recording");
      timerRef.current = window.setInterval(() => {
        const elapsed = Date.now() - startedAtRef.current;
        setElapsedMs(elapsed);
        if (elapsed >= maxRecordingMs) stopRecording();
      }, 200);
    } catch (recordError) {
      streamRef.current?.getTracks().forEach((track) => track.stop());
      setVoiceState("idle");
      setVoiceError(
        recordError instanceof DOMException && recordError.name === "NotAllowedError"
          ? "麦克风权限被拒绝，请在浏览器设置中允许后重试"
          : "无法启动录音",
      );
    }
  };

  const isRecording = voiceState === "recording";
  const micDisabled =
    voiceDisabled || !onVoiceCapture || voiceState === "requesting" || voiceState === "uploading";
  const micTitle =
    voiceTitle ??
    (isRecording ? "停止录音" : voiceState === "uploading" ? "正在转写…" : "语音输入");
  const confirmHint = pendingCapture ? voiceConfirmationHint(pendingCapture) : "";

  return (
    <div className="absolute bottom-[80px] left-0 right-0 z-30 bg-gradient-to-t from-[#F6F8F4] via-[#F6F8F4]/96 to-transparent px-5 pb-4 pt-8">
      {/* 快捷提示行；模型选择器放在同一行最左侧（在滚动容器之外，避免向上弹出的菜单被裁剪）。 */}
      <div className="flex items-center gap-2 mb-3">
        {/* 模型选择。当前仅前端选择与本地记忆，后端支持后再随请求下发。 */}
        <div className="relative flex-shrink-0">
          <button
            type="button"
            onClick={() => setModelMenuOpen((v) => !v)}
            className="flex items-center gap-1.5 rounded-full bg-white px-3 py-2 text-xs text-gray-600 shadow-[0_2px_12px_rgba(0,0,0,0.05)] hover:bg-gray-50 transition-colors"
          >
            <Sparkles size={13} className="text-primary" />
            <span className="font-medium">{selectedModel?.name}</span>
            <ChevronDown
              size={13}
              className={`opacity-50 transition-transform ${
                modelMenuOpen ? "rotate-180" : ""
              }`}
            />
          </button>

          {modelMenuOpen && (
            <>
              <div
                className="fixed inset-0 z-10"
                onClick={() => setModelMenuOpen(false)}
              />
              <div className="absolute bottom-full left-0 z-20 mb-1.5 w-56 overflow-hidden rounded-2xl border border-black/5 bg-white py-1 shadow-[0_8px_32px_rgba(0,0,0,0.12)]">
                {models.map((model) => {
                  const active = model.id === selectedModelID;
                  return (
                    <button
                      key={model.id}
                      type="button"
                      onClick={() => {
                        onSelectModel(model.id);
                        setModelMenuOpen(false);
                      }}
                      className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-gray-50"
                    >
                      <span className="min-w-0 flex-1">
                        <span className="block text-[13px] font-medium text-gray-800">
                          {model.name}
                        </span>
                        <span className="block truncate text-[11px] text-gray-400">
                          {model.desc}
                        </span>
                      </span>
                      {active && (
                        <Check size={15} className="flex-shrink-0 text-primary" />
                      )}
                    </button>
                  );
                })}
              </div>
            </>
          )}
        </div>

        {/* 快捷提示（可横向滚动） */}
        <div className="flex items-center gap-2 overflow-x-auto no-scrollbar">
          {PROMPTS.map((p) => (
            <button
              key={p.label}
              type="button"
              onClick={() => onSelectPrompt(p)}
              aria-pressed={activePrompt === p.label}
              className={`flex-shrink-0 flex items-center gap-1.5 rounded-full px-3.5 py-2 text-xs shadow-[0_2px_12px_rgba(0,0,0,0.05)] transition-colors ${
                activePrompt === p.label
                  ? "bg-primary text-primary-foreground"
                  : "bg-white text-gray-600 hover:bg-gray-50"
              }`}
            >
              <span>{p.emoji}</span>
              <span>{p.label}</span>
            </button>
          ))}
        </div>
      </div>

      {/* 语音状态条：录音中的计时、转写中的等待、失败提示、以及需要确认时的说明。 */}
      <AnimatePresence>
        {(isRecording || voiceState === "uploading" || voiceError || pendingCapture) && (
          <motion.div
            initial={{ opacity: 0, y: 6 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 6 }}
            transition={{ duration: 0.15 }}
            className="mb-2 rounded-2xl bg-white px-4 py-2.5 shadow-[0_2px_12px_rgba(0,0,0,0.05)]"
          >
            {isRecording ? (
              <div className="flex items-center gap-2 text-xs text-gray-600">
                <span className="h-2 w-2 flex-shrink-0 animate-pulse rounded-full bg-[#F4A460]" />
                <span>正在录音 {(elapsedMs / 1000).toFixed(1)}s</span>
                <span className="ml-auto text-gray-400">再次点击麦克风结束</span>
              </div>
            ) : voiceState === "uploading" ? (
              <div className="text-xs text-gray-500">正在转写…</div>
            ) : voiceError ? (
              <div className="flex items-start gap-2 text-xs text-[#C0603A]">
                <span className="flex-1">{voiceError}</span>
                <button
                  type="button"
                  onClick={() => setVoiceError("")}
                  aria-label="关闭提示"
                  className="flex-shrink-0 text-gray-400 hover:text-gray-600"
                >
                  <X size={13} />
                </button>
              </div>
            ) : (
              <div className="flex flex-col gap-1.5">
                <div className="flex items-start gap-2 text-xs text-gray-600">
                  <span className="flex-1">{confirmHint || "发送前先确认一下转写结果"}</span>
                  <button
                    type="button"
                    onClick={() => {
                      // 放弃这次语音关联：文字留在输入框里，用户当普通输入继续用。
                      setPendingCapture(null);
                    }}
                    aria-label="忽略提示"
                    className="flex-shrink-0 text-gray-400 hover:text-gray-600"
                  >
                    <X size={13} />
                  </button>
                </div>
                <label className="flex items-center gap-1.5 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={alwaysConfirm}
                    onChange={(e) => {
                      setAlwaysConfirm(e.target.checked);
                      setVoiceAlwaysConfirm(e.target.checked);
                    }}
                    className="h-3 w-3 accent-[#A8D5BA]"
                  />
                  每次语音都先确认再发送
                </label>
              </div>
            )}
          </motion.div>
        )}
      </AnimatePresence>

      <form onSubmit={handleSubmit} className="flex items-center gap-2.5">
        <input
          ref={fileInputRef}
          type="file"
          accept="image/*"
          capture="environment"
          className="hidden"
          onChange={handleFileChange}
        />
        <button
          type="button"
          onClick={() => fileInputRef.current?.click()}
          aria-label="拍照 / 上传图片"
          title="拍照 / 上传图片"
          className="w-11 h-11 rounded-full flex items-center justify-center shadow-[0_2px_16px_rgba(0,0,0,0.07)] bg-white text-gray-500 hover:bg-gray-50 transition-colors flex-shrink-0"
        >
          <Plus className="w-[20px] h-[20px]" />
        </button>

        <div className="flex-1 relative">
          <input
            ref={textInputRef}
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="输入想练习的知识点或面试问题..."
            className="w-full bg-white rounded-full px-5 py-3.5 pr-12 shadow-[0_2px_20px_rgba(0,0,0,0.06)] focus:outline-none focus:ring-2 focus:ring-[#A8D5BA]/30 transition-shadow text-sm"
          />
          <AnimatePresence>
            {input && (
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
            onClick={onStop}
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
              if (isRecording) {
                stopRecording();
                return;
              }
              void startRecording();
            }}
            disabled={micDisabled}
            aria-label={isRecording ? "停止录音" : "语音输入"}
            title={micTitle}
            className={`w-11 h-11 rounded-full flex items-center justify-center shadow-[0_2px_16px_rgba(0,0,0,0.07)] transition-colors flex-shrink-0 ${
              isRecording
                ? "bg-[#F4A460] text-white"
                : "bg-white text-gray-500 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-40"
            }`}
          >
            {isRecording ? <Square className="h-4 w-4" fill="currentColor" /> : <Mic className="w-[18px] h-[18px]" />}
          </motion.button>
        )}
      </form>
    </div>
  );
}
