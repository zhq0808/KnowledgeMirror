import { motion, AnimatePresence } from "motion/react";
import { BrainCircuit, Loader2, PauseCircle, RotateCcw } from "lucide-react";
import type { FeynmanPracticeState } from "../api/feynman";

interface PracticeStatusBarProps {
  state: FeynmanPracticeState | null;
}

const FREE_STATE_LABELS: Record<FeynmanPracticeState["state"], string> = {
  idle: "",
  awaiting_topic: "等你说一个想讲的主题",
  awaiting_answer: "正在练习 · 讲完直接发出来",
  analyzing_answer: "正在分析你的回答…",
  awaiting_follow_up: "已给出反馈 · 可以继续补充或换个主题",
  awaiting_retry: "需要重新回答",
  queue_paused: "练习已暂停 · 说“继续”回到这题",
};

function statusLabel(state: FeynmanPracticeState): string {
  if (!state.coach_task_id) return FREE_STATE_LABELS[state.state];
  if (state.state === "queue_paused") {
    return state.retry_required
      ? "教练处方已暂停 · 继续后必须重答原题"
      : "教练处方已暂停 · 说“继续”恢复";
  }
  if (state.state === "analyzing_answer") return "正在分析教练处方回答…";
  if (state.state === "awaiting_retry" || state.retry_required) {
    return "教练处方 · 必须重新完整回答原题";
  }
  return "教练处方 · 等待你的回答";
}

export function PracticeStatusBar({ state }: PracticeStatusBarProps) {
  const active = state != null && state.state !== "idle";
  const label = state ? statusLabel(state) : "";
  const question = state?.coach_task_id
    ? state.original_question || state.question
    : state?.question;
  const prescribed = Boolean(state?.coach_task_id);

  return (
    <AnimatePresence initial={false}>
      {active && state && (
        <motion.div
          initial={{ opacity: 0, height: 0 }}
          animate={{ opacity: 1, height: "auto" }}
          exit={{ opacity: 0, height: 0 }}
          className="overflow-hidden px-4"
          role="status"
          aria-live="polite"
        >
          <div className="flex items-start gap-2 rounded-2xl bg-[#F4EFE6] px-3 py-2 text-[#7A6248]">
            <span className="mt-[2px] flex-shrink-0" aria-hidden="true">
              {state.state === "analyzing_answer" ? (
                <Loader2 className="size-4 animate-spin" />
              ) : state.state === "queue_paused" ? (
                <PauseCircle className="size-4" />
              ) : state.state === "awaiting_retry" || state.retry_required ? (
                <RotateCcw className="size-4" />
              ) : (
                <BrainCircuit className="size-4" />
              )}
            </span>
            <div className="min-w-0 flex-1">
              <p className="text-xs leading-relaxed">
                {prescribed ? "每日教练" : "费曼练习"} · 第 {prescribed ? Math.max(state.round_no, 1) : Math.max(state.round_no, 0) + 1} 轮：{label}
              </p>
              {question ? (
                <p className="mt-[2px] break-words text-sm leading-relaxed text-[#5B4636]">
                  {question}
                </p>
              ) : null}
            </div>
          </div>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
