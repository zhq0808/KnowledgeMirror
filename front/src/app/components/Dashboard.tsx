import { useState } from "react";
import { motion } from "motion/react";
import {
  AlertCircle,
  BookOpenCheck,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Loader2,
  Play,
  RotateCcw,
  X,
} from "lucide-react";
import type { CoachGap, CoachProgress, CoachTask, CoachToday } from "../api/coach";
import {
  addLocalDays,
  calendarProgress,
  coachTaskCTA,
  coachTodayTasks,
  localDateKey,
} from "../lib/coach-view-model";
import { ProfileButton } from "./ProfileButton";

const WEEKDAYS = ["日", "一", "二", "三", "四", "五", "六"];

function localDate(value: string): Date {
  const [year, month, day] = value.split("-").map(Number);
  return new Date(year, month - 1, day);
}

function CalendarHeatmap({
  progress,
  disabled,
}: {
  progress: CoachProgress | null;
  disabled: boolean;
}) {
  const today = new Date();
  const [viewMonth, setViewMonth] = useState(
    new Date(today.getFullYear(), today.getMonth(), 1),
  );
  const firstDay = new Date(viewMonth.getFullYear(), viewMonth.getMonth(), 1);
  const calendarStart = addLocalDays(firstDay, -firstDay.getDay());
  const days = Array.from({ length: 42 }, (_, index) => addLocalDays(calendarStart, index));
  const progressByDate = calendarProgress(progress);
  const monthLabel = new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "long",
  }).format(viewMonth);
  const fromMonth = progress ? new Date(localDate(progress.from).getFullYear(), localDate(progress.from).getMonth(), 1) : null;
  const toMonth = progress ? new Date(localDate(progress.to).getFullYear(), localDate(progress.to).getMonth(), 1) : null;
  const canPrevious = fromMonth != null && viewMonth.getTime() > fromMonth.getTime();
  const canNext = toMonth != null && viewMonth.getTime() < toMonth.getTime();
  const changeMonth = (amount: number) => {
    setViewMonth(
      (current) => new Date(current.getFullYear(), current.getMonth() + amount, 1),
    );
  };

  return (
    <section aria-label="教练任务完成日历">
      <div className="mb-5 flex items-center justify-between px-1">
        <button
          type="button"
          onClick={() => changeMonth(-1)}
          disabled={disabled || !canPrevious}
          aria-label="上个月"
          className="flex size-8 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground disabled:cursor-not-allowed disabled:opacity-35"
        >
          <ChevronLeft size={16} />
        </button>
        <h3 className="text-[16px] font-semibold text-foreground">{monthLabel}</h3>
        <button
          type="button"
          onClick={() => changeMonth(1)}
          disabled={disabled || !canNext}
          aria-label="下个月"
          className="flex size-8 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground disabled:cursor-not-allowed disabled:opacity-35"
        >
          <ChevronRight size={16} />
        </button>
      </div>

      <div className="grid grid-cols-7 gap-y-1.5">
        {WEEKDAYS.map((weekday) => (
          <div key={weekday} className="pb-2 text-center text-[10px] font-medium text-muted-foreground">
            {weekday}
          </div>
        ))}
        {days.map((date) => {
          const key = localDateKey(date);
          const item = progressByDate[key];
          const outside = date.getMonth() !== viewMonth.getMonth();
          if (outside) return <div key={key} aria-hidden="true" className="h-8" />;

          const summary = item ? `${item.completed}/${item.total} 项教练任务完成` : "没有教练任务记录";
          const completed = item != null && item.total > 0 && item.completed === item.total;
          const partial = item != null && item.completed > 0 && !completed;
          return (
            <div
              key={key}
              title={`${date.getMonth() + 1}月${date.getDate()}日，${summary}`}
              aria-label={`${date.getMonth() + 1}月${date.getDate()}日，${summary}`}
              className="flex h-8 min-w-0 items-center justify-center"
            >
              <span
                className={`flex size-7 items-center justify-center rounded-full text-[11px] font-semibold ${
                  completed
                    ? "bg-primary text-white"
                    : partial
                      ? "border-2 border-primary bg-white text-primary"
                      : item?.total
                        ? "border border-[#B8C4BB] bg-secondary text-foreground"
                        : "text-muted-foreground"
                } ${key === localDateKey(today) ? "ring-2 ring-[#D59A2F] ring-offset-2" : ""}`}
              >
                {date.getDate()}
              </span>
            </div>
          );
        })}
      </div>
      <p className="mt-3 text-[10px] leading-4 text-muted-foreground">
        实心表示当天全部完成，描边表示部分完成，灰色表示有任务但尚未完成。
      </p>
    </section>
  );
}

interface DashboardProps {
  onClose?: () => void;
  onOpenProfile?: () => void;
  mode?: "drawer" | "page";
  coachEnabled: boolean;
  capabilityLoading: boolean;
  capabilityError: string | null;
  today: CoachToday | null;
  progress: CoachProgress | null;
  gaps: CoachGap[];
  gapsLoaded: boolean;
  gapsLoading: boolean;
  gapsError: string | null;
  loading: boolean;
  error: string | null;
  globalBusy: boolean;
  launchingTaskID: string | null;
  onRetry: () => void;
  onLaunchTask: (task: CoachTask) => void;
  onEmptyStateAction: () => void;
}

function TaskRow({
  task,
  launching,
  disabled,
  onLaunch,
}: {
  task: CoachTask;
  launching: boolean;
  disabled: boolean;
  onLaunch: () => void;
}) {
  const cta = coachTaskCTA(task.status);
  const role = task.plan_role === "required" ? "必做" : "选做";
  const kind = task.task_type === "feynman_retry" ? "薄弱点复测" : "知识点讲解";
  const completed = task.status === "completed";
  const retry = task.status === "awaiting_retry";

  return (
    <article className="rounded-lg border border-border bg-card px-3.5 py-3">
      <div className="flex items-start gap-3">
        <span
          className={`mt-0.5 flex size-8 flex-shrink-0 items-center justify-center rounded-lg ${
            completed ? "bg-[#E4F1E8] text-[#2E6941]" : retry ? "bg-[#FFF4D9] text-[#946B16]" : "bg-secondary text-primary"
          }`}
          aria-hidden="true"
        >
          {completed ? <CheckCircle2 size={16} /> : retry ? <RotateCcw size={16} /> : <BookOpenCheck size={16} />}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="rounded-full bg-secondary px-2 py-0.5 text-[10px] font-semibold text-primary">{role}</span>
            <span className="text-[10px] text-muted-foreground">{kind}</span>
          </div>
          <p className="mt-1.5 text-[13px] font-semibold leading-5 text-foreground">{task.question}</p>
        </div>
      </div>
      <div className="mt-3 flex justify-end">
        <button
          type="button"
          disabled={cta.disabled || launching || disabled}
          onClick={onLaunch}
          aria-label={`${cta.label}：${task.question}`}
          className={`inline-flex min-w-20 items-center justify-center gap-1.5 rounded-lg px-3 py-2 text-[12px] font-semibold transition-colors disabled:cursor-not-allowed disabled:opacity-60 ${
            cta.disabled
              ? "bg-secondary text-muted-foreground"
              : retry
                ? "bg-[#8B641A] text-white hover:bg-[#745315]"
                : "bg-primary text-white hover:bg-primary/90"
          }`}
        >
          {launching ? <Loader2 size={13} className="animate-spin" /> : !cta.disabled ? <Play size={13} /> : null}
          {launching ? "正在进入" : cta.label}
        </button>
      </div>
    </article>
  );
}

export function Dashboard({
  onClose,
  onOpenProfile,
  mode = "drawer",
  coachEnabled,
  capabilityLoading,
  capabilityError,
  today,
  progress,
  gaps,
  gapsLoaded,
  gapsLoading,
  gapsError,
  loading,
  error,
  globalBusy,
  launchingTaskID,
  onRetry,
  onLaunchTask,
  onEmptyStateAction,
}: DashboardProps) {
  const allTasks = coachTodayTasks(today);
  const tasks = allTasks.filter((task) => task.date === today?.date);
  const carriedOverTask = today?.active_task?.carried_over ? today.active_task : null;
  const completedTasks = tasks.filter((task) => task.status === "completed");
  const emptyAction = today?.empty_state?.action === "review_candidates" && today.empty_state.action_path === "/knowledge";

  return (
    <motion.aside
      key="learning-dashboard"
      initial={mode === "drawer" ? { x: "100%" } : { opacity: 0, y: 8 }}
      animate={mode === "drawer" ? { x: 0 } : { opacity: 1, y: 0 }}
      exit={mode === "drawer" ? { x: "100%" } : { opacity: 0 }}
      transition={mode === "drawer" ? { type: "spring", stiffness: 320, damping: 34 } : { duration: 0.2, ease: "easeOut" }}
      className={mode === "drawer"
        ? "absolute inset-y-0 right-0 z-50 flex w-[92%] max-w-[430px] flex-col overflow-hidden rounded-l-[28px] border-l border-white/80 bg-background shadow-[-18px_0_48px_rgba(24,45,32,0.18)]"
        : "relative flex min-h-0 flex-1 flex-col overflow-hidden bg-background pb-16"}
    >
      <div className="flex flex-shrink-0 items-center justify-between px-5 pb-4 pt-8">
        <div>
          <h2 className="text-[18px] font-semibold text-foreground">学习看板</h2>
          <p className="text-[12px] text-muted-foreground">服务端处方与真实完成记录</p>
        </div>
        <div className="flex items-center gap-2">
          {mode === "page" && onOpenProfile && <ProfileButton onClick={onOpenProfile} />}
          {mode === "drawer" && onClose && (
            <button type="button" onClick={onClose} disabled={globalBusy} aria-label="关闭学习看板" className="flex size-8 items-center justify-center rounded-full bg-secondary transition-colors hover:bg-accent disabled:opacity-50">
              <X size={15} className="text-muted-foreground" />
            </button>
          )}
        </div>
      </div>

      <div className={`flex-1 overflow-y-auto px-6 ${mode === "page" ? "pb-24" : "pb-8"}`} style={{ scrollbarWidth: "none" }}>
        {capabilityLoading ? (
          <div role="status" className="flex items-center justify-center gap-2 py-16 text-[13px] text-muted-foreground">
            <Loader2 size={16} className="animate-spin" /> 正在确认每日教练能力…
          </div>
        ) : capabilityError ? (
          <div role="alert" className="rounded-lg border border-destructive/30 bg-card p-5 text-center">
            <p className="text-[13px] text-foreground">{capabilityError}</p>
            <button type="button" onClick={onRetry} disabled={globalBusy} className="mt-3 rounded-lg bg-primary px-4 py-2 text-[12px] font-semibold text-white disabled:opacity-50">重新加载</button>
          </div>
        ) : !coachEnabled ? (
          <div role="status" className="rounded-lg border border-border bg-card p-5 text-center">
            <AlertCircle className="mx-auto text-muted-foreground" size={22} />
            <p className="mt-3 text-[13px] font-semibold text-foreground">每日教练当前不可用</p>
            <p className="mt-1 text-[11px] leading-5 text-muted-foreground">仍可前往知识库或使用普通练习。</p>
          </div>
        ) : loading && !today && !progress ? (
          <div role="status" className="flex items-center justify-center gap-2 py-16 text-[13px] text-muted-foreground">
            <Loader2 size={16} className="animate-spin" /> 正在加载教练计划…
          </div>
        ) : error && !today && !progress ? (
          <div role="alert" className="rounded-lg border border-destructive/30 bg-card p-5 text-center">
            <p className="text-[13px] text-foreground">{error}</p>
            <button type="button" onClick={onRetry} disabled={globalBusy} className="mt-3 rounded-lg bg-primary px-4 py-2 text-[12px] font-semibold text-white disabled:opacity-50">重新加载</button>
          </div>
        ) : (
          <>
            <CalendarHeatmap progress={progress} disabled={globalBusy} />
            <div className="my-5 h-px bg-border" />

            <section aria-labelledby="today-todo-title">
              <div className="mb-4 flex items-end justify-between gap-3">
                <div>
                  <p className="text-[11px] font-medium text-primary">{today?.date ?? "今天"}</p>
                  <h3 id="today-todo-title" className="mt-0.5 text-[17px] font-semibold text-foreground">今日教练任务</h3>
                </div>
                <p className="text-right text-[11px] text-muted-foreground">
                  <strong className="block text-[13px] text-foreground">{completedTasks.length}/{tasks.length}</strong>
                  服务端完成记录
                </p>
              </div>

              {error && (
                <div role="alert" className="mb-3 flex items-center justify-between gap-3 rounded-lg bg-[#FFF4D9] px-3 py-2 text-[11px] text-[#765514]">
                  <span>{error}</span>
                  <button type="button" onClick={onRetry} disabled={globalBusy} className="font-semibold underline disabled:opacity-50">重试</button>
                </div>
              )}

              {carriedOverTask && (
                <div className="mb-4">
                  <p className="mb-2 text-[11px] font-semibold text-[#8B5D28]">延续中的教练任务</p>
                  <TaskRow task={carriedOverTask} launching={launchingTaskID === carriedOverTask.coach_task_id} disabled={globalBusy} onLaunch={() => onLaunchTask(carriedOverTask)} />
                </div>
              )}

              {tasks.length > 0 ? (
                <div className="space-y-2">
                  {tasks.map((task) => (
                    <TaskRow key={task.coach_task_id} task={task} launching={launchingTaskID === task.coach_task_id} disabled={globalBusy} onLaunch={() => onLaunchTask(task)} />
                  ))}
                </div>
              ) : (
                <div className="rounded-lg border border-border bg-card p-5 text-center">
                  <p className="text-[13px] font-semibold text-foreground">今天暂无可安排的任务</p>
                  <p className="mt-1 text-[11px] leading-5 text-muted-foreground">{today?.empty_state?.message ?? "今天没有教练任务。"}</p>
                  {emptyAction && (
                    <button type="button" onClick={onEmptyStateAction} disabled={globalBusy} className="mt-4 rounded-lg bg-primary px-3 py-2 text-[12px] font-semibold text-white disabled:opacity-50">
                      前往知识库
                    </button>
                  )}
                </div>
              )}
            </section>

            <div className="my-5 h-px bg-border" />

            <section aria-labelledby="daily-review-title">
              <h3 id="daily-review-title" className="text-[17px] font-semibold text-foreground">今日事实复盘</h3>
              <div className="mt-3 overflow-hidden rounded-lg border border-border bg-card">
                <div className="border-b border-border px-4 py-4">
                  <h4 className="text-[12px] font-semibold text-foreground">已完成处方</h4>
                  {completedTasks.length ? (
                    <ul className="mt-2 space-y-2">
                      {completedTasks.map((task) => <li key={task.coach_task_id} className="text-[11px] leading-5 text-muted-foreground">{task.question}</li>)}
                    </ul>
                  ) : (
                    <p className="mt-2 text-[11px] text-muted-foreground">今天还没有完成教练处方。</p>
                  )}
                </div>
                <div className="px-4 py-4">
                  <h4 className="text-[12px] font-semibold text-foreground">当前待补强（前 50 项）</h4>
                  {gapsLoading && !gapsLoaded ? (
                    <p className="mt-2 flex items-center gap-1.5 text-[11px] text-muted-foreground"><Loader2 size={12} className="animate-spin" /> 正在加载薄弱点…</p>
                  ) : gapsError && !gapsLoaded ? (
                    <p className="mt-2 text-[11px] text-[#765514]">{gapsError}</p>
                  ) : gapsLoaded && gaps.length ? (
                    <ul className="mt-2 space-y-2">
                      {gaps.slice(0, 3).map((gap) => <li key={gap.gap_id} className="text-[11px] leading-5 text-muted-foreground">{gap.title}</li>)}
                    </ul>
                  ) : gapsLoaded ? (
                    <p className="mt-2 text-[11px] text-muted-foreground">当前没有开放薄弱点。</p>
                  ) : (
                    <p className="mt-2 text-[11px] text-muted-foreground">进入看板后加载薄弱点。</p>
                  )}
                  {gapsError && gapsLoaded && <p className="mt-2 text-[10px] text-[#765514]">刷新失败：{gapsError}</p>}
                </div>
              </div>
              <p className="mt-2.5 text-[10px] leading-4 text-muted-foreground">这里只展示今日任务状态和当前开放薄弱点；接口未提供输出次数，因此不推算输出量。</p>
            </section>
          </>
        )}
      </div>
    </motion.aside>
  );
}
