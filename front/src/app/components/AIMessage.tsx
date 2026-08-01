import { motion } from "motion/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { BookOpen, LoaderCircle, RotateCcw, ShieldAlert, Square, Volume2 } from "lucide-react";
import type { RetrievalSources } from "../api/chat";
import { useSpeechPlayback } from "../lib/useSpeechPlayback";

interface AIMessageProps {
  message: string;
  time?: string;
  // failed 为 true 时该气泡对应一次失败发送，展示重试入口。
  failed?: boolean;
  // onRetry 复用原 client_message_id 重新发送；仅失败气泡提供。
  onRetry?: () => void;
  retrieval?: RetrievalSources;
  speechEnabled?: boolean;
}

// AIMessage 渲染助手回复。回复内容可能包含 markdown（加粗、有序/无序列表、链接等），
// 用 react-markdown + remark-gfm 渲染，并通过 arbitrary variant 给嵌套元素补样式，
// 避免额外引入 typography 插件。
// 当内容为空（回复尚未吐字）时，显示三个主题绿色的跳动圆点作为“正在输入”指示。
export function AIMessage({ message, time, failed, onRetry, retrieval, speechEnabled = false }: AIMessageProps) {
  const isTyping = message.trim().length === 0;
  const speech = useSpeechPlayback();

  return (
    <motion.div
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      className="flex flex-col items-start mb-4 px-5"
    >
      <div className="bg-white rounded-2xl rounded-tl-md px-4 py-3 max-w-[80%] shadow-[0_2px_20px_rgba(0,0,0,0.04)]">
        {isTyping ? (
          <div className="flex items-center gap-1 py-0.5">
            {[0, 0.18, 0.36].map((delay, i) => (
              <motion.span
                key={i}
                className="w-1.5 h-1.5 rounded-full bg-primary"
                animate={{ opacity: [0.3, 1, 0.3], y: [0, -3, 0] }}
                transition={{
                  duration: 1.1,
                  repeat: Infinity,
                  delay,
                  ease: "easeInOut",
                }}
              />
            ))}
          </div>
        ) : (
          <>
            {retrieval && retrieval.sources.length > 0 && (
              <div className="mb-3 border-b border-border pb-3">
                <div className="mb-2 flex items-center gap-1.5 text-[11px] font-semibold text-foreground">
                  <BookOpen size={13} className="text-primary" />
                  本轮引用 {retrieval.sources.length} 个资料片段
                </div>
                <div className="space-y-1.5">
                  {retrieval.sources.map((source) => (
                    <div key={source.source_chunk_id} className="text-[11px] leading-4 text-muted-foreground">
                      <span className="font-semibold text-primary">[{source.ref}]</span>{" "}
                      <span className="font-medium text-foreground">《{source.document_title}》v{source.version_no}</span>
                      {source.heading_path.length > 0 && ` · ${source.heading_path.join(" > ")}`}
                      <span className="ml-1">· {source.origin_label} · {source.trust_label}</span>
                      {source.truncated && <span className="ml-1 text-amber-700">· 已截断</span>}
                    </div>
                  ))}
                </div>
              </div>
            )}
            {retrieval && retrieval.quarantined_count > 0 && (
              <div className="mb-3 flex items-center gap-1.5 text-[11px] text-amber-700">
                <ShieldAlert size={13} />
                {retrieval.quarantined_count} 个疑似指令注入片段已隔离
              </div>
            )}
            <div className="text-sm leading-relaxed text-gray-700 [&_a]:text-primary [&_a]:underline [&_code]:rounded [&_code]:bg-gray-100 [&_code]:px-1 [&_code]:py-0.5 [&_code]:text-[13px] [&_h1]:mb-1 [&_h1]:text-base [&_h1]:font-semibold [&_h2]:mb-1 [&_h2]:font-semibold [&_h3]:mb-1 [&_h3]:font-semibold [&_li]:mb-1 [&_ol]:my-2 [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:mb-2 [&_p:last-child]:mb-0 [&_strong]:font-semibold [&_strong]:text-gray-900 [&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-5">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{message}</ReactMarkdown>
            </div>
          </>
        )}
      </div>
      {!isTyping && (
        <div className="mt-1 flex items-center gap-2 pl-1">
          {/* 朗读按钮：把提问真正“说”出来，复现「实时 + 用耳朵听 + 有人等」的面试条件。
              念的是屏幕上这段原文，听到的和看到的永远一致。 */}
          {speechEnabled && (
            <button
              type="button"
              onClick={() => speech.toggle(message)}
              disabled={speech.status === "loading"}
              title={speech.status === "idle" ? "朗读这段" : "停止朗读"}
              aria-label={speech.status === "idle" ? "朗读这段" : "停止朗读"}
              className="flex size-7 items-center justify-center rounded-lg text-[11px] text-muted-foreground transition-colors hover:text-primary disabled:opacity-50"
            >
              {speech.status === "loading" ? (
                <LoaderCircle size={12} className="animate-spin" />
              ) : speech.status === "playing" ? (
                <Square size={12} className="text-primary" />
              ) : (
                <Volume2 size={12} />
              )}
            </button>
          )}
          {time && <span className="text-[11px] text-muted-foreground">{time}</span>}
          {speech.error && <span className="text-[11px] text-amber-700">{speech.error}</span>}
        </div>
      )}
      {failed && onRetry && (
        <button
          type="button"
          onClick={onRetry}
          className="mt-1.5 flex items-center gap-1 rounded-lg px-1 text-[12px] text-primary hover:underline"
        >
          <RotateCcw size={12} />
          重试
        </button>
      )}
    </motion.div>
  );
}
