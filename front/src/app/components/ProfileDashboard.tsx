import { ArrowLeft, AlertCircle, Loader2, RotateCcw } from "lucide-react";
import type { CoachGap, CoachProgress } from "../api/coach";
import { GAP_LABELS, groupGaps } from "../lib/coach-view-model";

interface ProfileDashboardProps {
  onBack: () => void;
  coachEnabled: boolean;
  capabilityLoading: boolean;
  capabilityError: string | null;
  progress: CoachProgress | null;
  gaps: CoachGap[];
  gapsLoaded: boolean;
  gapsLoading: boolean;
  gapsError: string | null;
  loading: boolean;
  error: string | null;
  globalBusy: boolean;
  onRetry: () => void;
}

const RECENT_DATE_FORMATTER = new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric" });

function formatRecentDate(value: string): string {
  return RECENT_DATE_FORMATTER.format(new Date(value));
}

export function ProfileDashboard({
  onBack,
  coachEnabled,
  capabilityLoading,
  capabilityError,
  progress,
  gaps,
  gapsLoaded,
  gapsLoading,
  gapsError,
  loading,
  error,
  globalBusy,
  onRetry,
}: ProfileDashboardProps) {
  const grouped = groupGaps(gaps);

  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background">
      <header className="flex flex-shrink-0 items-center justify-between border-b border-border px-5 pb-4 pt-6">
        <div className="flex items-center gap-3">
          <button type="button" onClick={onBack} disabled={globalBusy} aria-label="返回" className="flex size-9 items-center justify-center rounded-full bg-secondary text-primary transition-colors hover:bg-accent disabled:opacity-50">
            <ArrowLeft size={18} />
          </button>
          <div>
            <h1 className="text-[20px] font-semibold leading-tight text-foreground">我的教练进展</h1>
            <p className="mt-0.5 text-[12px] text-muted-foreground">任务完成与开放薄弱点</p>
          </div>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto px-5 pb-8 pt-5" style={{ scrollbarWidth: "none" }}>
        {capabilityLoading ? (
          <div role="status" className="flex items-center justify-center gap-2 py-16 text-[13px] text-muted-foreground"><Loader2 size={16} className="animate-spin" /> 正在确认每日教练能力…</div>
        ) : capabilityError ? (
          <div role="alert" className="rounded-lg border border-destructive/30 bg-card p-6 text-center">
            <p className="text-[13px] text-foreground">{capabilityError}</p>
            <button type="button" onClick={onRetry} disabled={globalBusy} className="mt-3 inline-flex items-center gap-1.5 rounded-lg bg-primary px-4 py-2 text-[12px] font-semibold text-white disabled:opacity-50"><RotateCcw size={13} /> 重新加载</button>
          </div>
        ) : !coachEnabled ? (
          <div role="status" className="rounded-lg border border-border bg-card p-6 text-center">
            <AlertCircle className="mx-auto text-muted-foreground" size={22} />
            <p className="mt-3 text-[13px] font-semibold text-foreground">每日教练当前不可用</p>
          </div>
        ) : loading && !progress ? (
          <div role="status" className="flex items-center justify-center gap-2 py-16 text-[13px] text-muted-foreground"><Loader2 size={16} className="animate-spin" /> 正在加载教练进展…</div>
        ) : error && !progress ? (
          <div role="alert" className="rounded-lg border border-destructive/30 bg-card p-6 text-center">
            <p className="text-[13px] text-foreground">{error}</p>
            <button type="button" onClick={onRetry} disabled={globalBusy} className="mt-3 inline-flex items-center gap-1.5 rounded-lg bg-primary px-4 py-2 text-[12px] font-semibold text-white disabled:opacity-50"><RotateCcw size={13} /> 重新加载</button>
          </div>
        ) : (
          <>
            {error && <div role="alert" className="mb-4 rounded-lg bg-[#FFF4D9] px-3 py-2 text-[11px] text-[#765514]">{error} <button type="button" onClick={onRetry} disabled={globalBusy} className="font-semibold underline disabled:opacity-50">重试</button></div>}

            <section aria-labelledby="completion-title">
              <h2 id="completion-title" className="text-[17px] font-semibold text-foreground">任务完成</h2>
              <p className="mt-1 text-[11px] text-muted-foreground">统计区间 {progress?.from ?? "—"} 至 {progress?.to ?? "—"}</p>
              <div className="mt-3 grid grid-cols-2 gap-3">
                <div className="rounded-lg border border-border bg-card p-4">
                  <p className="text-[11px] font-medium text-muted-foreground">必做完成</p>
                  <p className="mt-2 text-[24px] font-semibold leading-none text-foreground">{progress?.required_completed ?? 0}<span className="text-[13px] font-normal text-muted-foreground">/{progress?.required_total ?? 0}</span></p>
                </div>
                <div className="rounded-lg border border-border bg-card p-4">
                  <p className="text-[11px] font-medium text-muted-foreground">选做完成</p>
                  <p className="mt-2 text-[24px] font-semibold leading-none text-foreground">{progress?.optional_completed ?? 0}<span className="text-[13px] font-normal text-muted-foreground">/{progress?.optional_total ?? 0}</span></p>
                </div>
              </div>
            </section>

            <section aria-labelledby="gap-groups-title" className="mt-7">
              <div className="flex items-end justify-between gap-3">
                <div>
                  <p className="text-[11px] font-medium text-[#8B5D28]">{gapsLoaded ? `前 50 项中共 ${gaps.length} 项` : "开放薄弱点（前 50 项）"}</p>
                  <h2 id="gap-groups-title" className="mt-0.5 text-[17px] font-semibold text-foreground">开放薄弱点分类</h2>
                </div>
              </div>
              {gapsLoading && !gapsLoaded ? (
                <p role="status" className="mt-3 flex items-center gap-2 rounded-lg border border-border bg-card p-5 text-[12px] text-muted-foreground"><Loader2 size={14} className="animate-spin" /> 正在加载薄弱点…</p>
              ) : gapsError && !gapsLoaded ? (
                <div role="alert" className="mt-3 rounded-lg border border-destructive/30 bg-card p-5 text-center text-[12px] text-foreground">{gapsError}<button type="button" onClick={onRetry} disabled={globalBusy} className="ml-2 font-semibold text-primary underline disabled:opacity-50">重试</button></div>
              ) : gapsLoaded ? (
                <div className="mt-3 grid grid-cols-2 gap-2">
                  {Object.entries(grouped).map(([type, items]) => (
                    <div key={type} className="rounded-lg border border-border bg-card p-3.5">
                      <p className="text-[11px] leading-4 text-muted-foreground">{GAP_LABELS[type as keyof typeof GAP_LABELS]}</p>
                      <p className="mt-2 text-[22px] font-semibold leading-none text-foreground">{items.length}</p>
                    </div>
                  ))}
                </div>
              ) : null}
              {gapsError && gapsLoaded && <p role="alert" className="mt-2 text-[11px] text-[#765514]">刷新失败：{gapsError}</p>}
            </section>

            {gapsLoaded && (
              <section aria-labelledby="gap-list-title" className="mt-7">
                <h2 id="gap-list-title" className="text-[17px] font-semibold text-foreground">当前薄弱点（前 50 项）</h2>
                {gaps.length ? (
                  <div className="mt-3 divide-y divide-border border-y border-border">
                    {gaps.map((gap) => (
                      <article key={gap.gap_id} className="py-3.5">
                        <div className="flex items-start justify-between gap-3">
                          <div className="min-w-0">
                            <p className="text-[13px] font-semibold text-foreground">{gap.title}</p>
                            <p className="mt-1 text-[11px] leading-5 text-muted-foreground">{gap.description}</p>
                          </div>
                          <span className="flex-shrink-0 rounded-full bg-secondary px-2 py-1 text-[10px] font-medium text-primary">{GAP_LABELS[gap.gap_type]}</span>
                        </div>
                        <p className="mt-2 text-[10px] text-muted-foreground">首次 {formatRecentDate(gap.first_seen_at)} · 最近 {formatRecentDate(gap.last_seen_at)}</p>
                      </article>
                    ))}
                  </div>
                ) : (
                  <p className="mt-3 rounded-lg border border-border bg-card p-5 text-center text-[12px] text-muted-foreground">当前没有开放薄弱点。</p>
                )}
              </section>
            )}

            <p className="mt-6 text-[10px] leading-4 text-muted-foreground">当前接口不提供 JD 覆盖率、掌握等级、首答通过率或复测总量，因此此页不展示这些推算指标。</p>
          </>
        )}
      </div>
    </main>
  );
}
