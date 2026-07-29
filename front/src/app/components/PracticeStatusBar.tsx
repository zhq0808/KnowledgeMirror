import { motion, AnimatePresence } from "motion/react";
import { BrainCircuit, Loader2, PauseCircle } from "lucide-react";
import type { FeynmanPracticeState } from "../api/feynman";

interface PracticeStatusBarProps {
  state: FeynmanPracticeState | null;
}

// 状态条只做一件事：让用户随时知道“现在是不是在练习、这题是什么”。
// 它没有任何按钮——开始、暂停、跳过、结束全部用自然语言在输入框里说，
// 一旦这里出现操作按钮，练习就又变回了一个需要点来点去的独立模式。
const STATE_LABELS: Record<FeynmanPracticeState["state"], string> = {
  idle: "",
  awaiting_topic: "等你说一个想讲的主题",
  awaiting_answer: "正在练习 · 讲完直接发出来",
  analyzing_answer: "正在看你讲的内容…",
  awaiting_follow_up: "已给出反馈 · 可以继续补充或换个主题",
  queue_paused: "练习已暂停 · 说“继续”回到这题",
};

export function PracticeStatusBar({ state }: PracticeStatusBarProps) {
  const active = state != null && state.state !== "idle";
  const label = state ? STATE_LABELS[state.state] : "";

  return (
    <AnimatePresence initial={false}>
      {active && (
        <motion.div
          initial={{ opacity: 0, height: 0 }}
          animate={{ opacity: 1, height: "auto" }}
          exit={{ opacity: 0, height: 0 }}
          className="px-4 overflow-hidden"
        >
          <div className="flex items-start gap-2 rounded-2xl bg-[#F4EFE6] px-3 py-2 text-[#7A6248]">
            <span className="mt-[2px] flex-shrink-0">
              {state?.state === "analyzing_answer" ? (
                <Loader2 className="w-4 h-4 animate-spin" />
              ) : state?.state === "queue_paused" ? (
                <PauseCircle className="w-4 h-4" />
              ) : (
                <BrainCircuit className="w-4 h-4" />
              )}
            </span>
            <div className="min-w-0 flex-1">
              <p className="text-xs leading-relaxed">
                费曼练习 · 第 {Math.max(state?.round_no ?? 0, 0) + 1} 轮：{label}
              </p>
              {state?.question ? (
                <p className="mt-[2px] text-sm leading-relaxed text-[#5B4636] break-words">
                  {state.question}
                </p>
              ) : null}
            </div>
          </div>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
