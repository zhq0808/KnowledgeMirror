import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from "react";
import {
  Check,
  FileText,
  LoaderCircle,
  RefreshCw,
  ShieldCheck,
  X,
} from "lucide-react";
import { listKnowledgePoints, APIError, type KnowledgePoint } from "../api/knowledge";
import {
  confirmFeynmanTranscript,
  createFeynmanAttempt,
  decideFeynmanEvaluation,
  evaluateFeynmanAttempt,
  getFeynmanAttempt,
  getFeynmanEvaluation,
  getFeynmanRubric,
  newFeynmanIdempotencyKey,
  uploadFeynmanAudio,
  type EvaluationPayload,
  type FeynmanAttempt,
  type FeynmanEvaluation,
  type FeynmanRubric,
} from "../api/feynman";
import { encodeRecordingAsWav, recordingUnavailableReason } from "../lib/audio";
import { Button } from "./ui/button";
import { Textarea } from "./ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";

interface FeynmanPracticePanelProps {
  onRecordingChange?: (recording: boolean) => void;
  onVoiceAvailabilityChange?: (available: boolean) => void;
}

export interface FeynmanPracticeHandle {
  toggleRecording: () => void;
}

const selectedPointStorageKey = "feynman_selected_knowledge_point";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

function attemptStorageKey(knowledgePointID: string): string {
  return `feynman_attempt_${knowledgePointID}`;
}

function idempotencyStorageKey(knowledgePointID: string): string {
  return `feynman_idempotency_${knowledgePointID}`;
}

function clonePayload(payload: EvaluationPayload): EvaluationPayload {
  return JSON.parse(JSON.stringify(payload)) as EvaluationPayload;
}

export const FeynmanPracticePanel = forwardRef<FeynmanPracticeHandle, FeynmanPracticePanelProps>(function FeynmanPracticePanel({
  onRecordingChange,
  onVoiceAvailabilityChange,
}, ref) {
  const [points, setPoints] = useState<KnowledgePoint[]>([]);
  const [selectedPointID, setSelectedPointID] = useState("");
  const [rubric, setRubric] = useState<FeynmanRubric | null>(null);
  const [attempt, setAttempt] = useState<FeynmanAttempt | null>(null);
  const [evaluation, setEvaluation] = useState<FeynmanEvaluation | null>(null);
  const [evaluationDraft, setEvaluationDraft] = useState<EvaluationPayload | null>(null);
  const [transcriptDraft, setTranscriptDraft] = useState("");
  const [decisionNote, setDecisionNote] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [isRecording, setIsRecording] = useState(false);
  const [elapsedMs, setElapsedMs] = useState(0);

  const recorderRef = useRef<MediaRecorder | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const chunksRef = useRef<Blob[]>([]);
  const startedAtRef = useRef(0);
  const timerRef = useRef<number | null>(null);
  const pointerRecordingRef = useRef(false);
  const requestingMicrophoneRef = useRef(false);

  useEffect(() => {
    let cancelled = false;
    listKnowledgePoints()
      .then((items) => {
        if (cancelled) return;
        const activePoints = items.filter((item) => item.status === "active");
        setPoints(activePoints);
        const storedPointID = localStorage.getItem(selectedPointStorageKey);
        if (storedPointID && activePoints.some((item) => item.knowledge_point_id === storedPointID)) {
          void selectPoint(storedPointID);
        }
      })
      .catch((loadError) => {
        if (!cancelled) setError(errorMessage(loadError, "加载知识点失败"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
      if (timerRef.current !== null) window.clearInterval(timerRef.current);
      if (recorderRef.current && recorderRef.current.state !== "inactive") {
        recorderRef.current.ondataavailable = null;
        recorderRef.current.onstop = null;
        recorderRef.current.stop();
      }
      streamRef.current?.getTracks().forEach((track) => track.stop());
      onRecordingChange?.(false);
    };
  }, []);

  useEffect(() => {
    if (attempt?.status !== "transcribing") return;
    const timer = window.setTimeout(async () => {
      try {
        const refreshed = await getFeynmanAttempt(attempt.attempt_id);
        setAttempt(refreshed);
        if (refreshed.active_audio_task?.raw_transcript) {
          setTranscriptDraft(refreshed.active_audio_task.raw_transcript);
        }
      } catch {
        // 下一轮或用户主动重试时再恢复，避免轮询错误覆盖主操作提示。
      }
    }, 1800);
    return () => window.clearTimeout(timer);
  }, [attempt]);

  const loadEvaluation = async (attemptID: string) => {
    try {
      const result = await getFeynmanEvaluation(attemptID);
      setEvaluation(result);
      setEvaluationDraft(result.decision?.final_payload ?? (result.payload ? clonePayload(result.payload) : null));
    } catch (loadError) {
      if (!(loadError instanceof APIError && loadError.status === 404)) throw loadError;
    }
  };

  const selectPoint = async (knowledgePointID: string) => {
    localStorage.setItem(selectedPointStorageKey, knowledgePointID);
    setSelectedPointID(knowledgePointID);
    setRubric(null);
    setAttempt(null);
    setEvaluation(null);
    setEvaluationDraft(null);
    setTranscriptDraft("");
    setDecisionNote("");
    setError("");
    setBusy("loading-topic");
    try {
      const rubricResult = await getFeynmanRubric(knowledgePointID);
      setRubric(rubricResult);
      const storedAttemptID = localStorage.getItem(attemptStorageKey(knowledgePointID));
      if (storedAttemptID) {
        try {
          const restored = await getFeynmanAttempt(storedAttemptID);
          setAttempt(restored);
          setTranscriptDraft(
            restored.confirmation?.confirmed_transcript ?? restored.active_audio_task?.raw_transcript ?? "",
          );
          if (restored.confirmation) await loadEvaluation(restored.attempt_id);
        } catch (restoreError) {
          if (restoreError instanceof APIError && restoreError.status === 404) {
            localStorage.removeItem(attemptStorageKey(knowledgePointID));
          } else {
            throw restoreError;
          }
        }
      }
    } catch (loadError) {
      setError(errorMessage(loadError, "加载练习主题失败"));
    } finally {
      setBusy("");
    }
  };

  const ensureAttempt = async (): Promise<FeynmanAttempt> => {
    if (attempt) return attempt;
    let idempotencyKey = localStorage.getItem(idempotencyStorageKey(selectedPointID));
    if (!idempotencyKey) {
      idempotencyKey = newFeynmanIdempotencyKey();
      localStorage.setItem(idempotencyStorageKey(selectedPointID), idempotencyKey);
    }
    const created = await createFeynmanAttempt(selectedPointID, idempotencyKey);
    localStorage.setItem(attemptStorageKey(selectedPointID), created.attempt_id);
    setAttempt(created);
    return created;
  };

  const uploadRecording = async (blob: Blob, durationMs: number) => {
    setBusy("uploading");
    setError("");
    try {
      // 浏览器只能录出 webm/opus 这类压缩格式，而 STT 供应商只收 wav/mp3，
      // 所以这里必须先转一道码再上传；失败时抛出的就是给用户看的话。
      const wav = await encodeRecordingAsWav(blob);
      const current = await ensureAttempt();
      const result = await uploadFeynmanAudio(current.attempt_id, wav, durationMs);
      setAttempt(result);
      setTranscriptDraft(result.active_audio_task?.raw_transcript ?? "");
      if (result.status === "failed") {
        setError(result.active_audio_task?.transcript_error || "转写失败，请重新录音");
      }
    } catch (uploadError) {
      setError(errorMessage(uploadError, "上传录音失败"));
    } finally {
      setBusy("");
    }
  };

  const stopRecording = () => {
    pointerRecordingRef.current = false;
    const recorder = recorderRef.current;
    if (recorder && recorder.state !== "inactive") recorder.stop();
  };

  const startRecording = async () => {
    if (!selectedPointID || busy || isRecording || attempt?.confirmation) return;
    const unavailable = recordingUnavailableReason();
    if (unavailable) {
      setError(unavailable);
      return;
    }
    pointerRecordingRef.current = true;
    requestingMicrophoneRef.current = true;
    setError("");
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      requestingMicrophoneRef.current = false;
      if (!pointerRecordingRef.current) {
        stream.getTracks().forEach((track) => track.stop());
        return;
      }
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
        setIsRecording(false);
        stream.getTracks().forEach((track) => track.stop());
        const blob = new Blob(chunksRef.current, { type: recorder.mimeType || chunksRef.current[0]?.type || "audio/webm" });
        if (blob.size > 0) void uploadRecording(blob, durationMs);
      };
      startedAtRef.current = Date.now();
      setElapsedMs(0);
      recorder.start(250);
      setIsRecording(true);
      timerRef.current = window.setInterval(() => {
        const elapsed = Date.now() - startedAtRef.current;
        setElapsedMs(elapsed);
        if (elapsed >= 180000) stopRecording();
      }, 200);
    } catch (recordError) {
      requestingMicrophoneRef.current = false;
      streamRef.current?.getTracks().forEach((track) => track.stop());
      setError(recordError instanceof DOMException && recordError.name === "NotAllowedError"
        ? "麦克风权限被拒绝，请在浏览器设置中允许后重试"
        : errorMessage(recordError, "无法启动录音"));
    }
  };

  const confirmTranscript = async () => {
    if (!attempt || !transcriptDraft.trim()) return;
    setBusy("confirming");
    setError("");
    try {
      const confirmed = await confirmFeynmanTranscript(attempt.attempt_id, transcriptDraft);
      setAttempt(confirmed);
      setBusy("evaluating");
      const result = await evaluateFeynmanAttempt(attempt.attempt_id);
      setEvaluation(result);
      setEvaluationDraft(result.payload ? clonePayload(result.payload) : null);
    } catch (confirmError) {
      setError(errorMessage(confirmError, "确认或评估失败"));
      try {
        const restored = await getFeynmanAttempt(attempt.attempt_id);
        setAttempt(restored);
        if (restored.confirmation) await loadEvaluation(restored.attempt_id);
      } catch {
        // 保留主错误。
      }
    } finally {
      setBusy("");
    }
  };

  const retryEvaluation = async () => {
    if (!attempt) return;
    setBusy("evaluating");
    setError("");
    try {
      const result = await evaluateFeynmanAttempt(attempt.attempt_id);
      setEvaluation(result);
      setEvaluationDraft(result.payload ? clonePayload(result.payload) : null);
    } catch (evaluationError) {
      setError(errorMessage(evaluationError, "评估失败，请重试"));
      try { await loadEvaluation(attempt.attempt_id); } catch { /* 保留主错误 */ }
    } finally {
      setBusy("");
    }
  };

  const decide = async (decision: "confirmed" | "rejected") => {
    if (!evaluation || (decision === "confirmed" && !evaluationDraft)) return;
    setBusy("deciding");
    setError("");
    try {
      const result = await decideFeynmanEvaluation(
        evaluation.evaluation_id,
        decision,
        decision === "confirmed" ? evaluationDraft ?? undefined : undefined,
        decisionNote,
      );
      setEvaluation(result);
      setEvaluationDraft(result.decision?.final_payload ?? evaluationDraft);
    } catch (decisionError) {
      setError(errorMessage(decisionError, "处理评估结果失败"));
    } finally {
      setBusy("");
    }
  };

  const startNewAttempt = () => {
    if (!selectedPointID) return;
    localStorage.removeItem(attemptStorageKey(selectedPointID));
    localStorage.removeItem(idempotencyStorageKey(selectedPointID));
    setAttempt(null);
    setEvaluation(null);
    setEvaluationDraft(null);
    setTranscriptDraft("");
    setDecisionNote("");
    setError("");
  };

  const selectedPoint = points.find((item) => item.knowledge_point_id === selectedPointID);
  const canRecord = Boolean(selectedPointID && !attempt?.confirmation && !busy);
  const evaluationResolved = Boolean(evaluation?.decision);

  useImperativeHandle(ref, () => ({
    toggleRecording: () => {
      if (isRecording || requestingMicrophoneRef.current) {
        stopRecording();
        return;
      }
      void startRecording();
    },
  }), [attempt, busy, isRecording, selectedPointID]);

  useEffect(() => {
    onRecordingChange?.(isRecording);
  }, [isRecording, onRecordingChange]);

  useEffect(() => {
    onVoiceAvailabilityChange?.(canRecord || isRecording || requestingMicrophoneRef.current);
  }, [canRecord, isRecording, onVoiceAvailabilityChange]);

  return (
    <div className="pb-52">
      <main className="mx-auto max-w-3xl px-5 py-6">
        <div className="mb-5">
          <h1 className="text-lg font-semibold text-foreground">费曼学习</h1>
          <p className="mt-1 text-xs text-muted-foreground">选择知识点后，点击下方麦克风开始输出</p>
        </div>
        <section className="border-b border-border pb-6">
          <label className="mb-2 block text-sm font-medium text-foreground">练习主题</label>
          <Select value={selectedPointID} onValueChange={(value) => void selectPoint(value)} disabled={loading || Boolean(busy)}>
            <SelectTrigger className="h-11"><SelectValue placeholder={loading ? "正在加载知识点" : "选择一个知识点"} /></SelectTrigger>
            <SelectContent>
              {points.map((point) => <SelectItem key={point.knowledge_point_id} value={point.knowledge_point_id}>{point.title}</SelectItem>)}
            </SelectContent>
          </Select>
          {!loading && points.length === 0 && <p className="mt-2 text-sm text-muted-foreground">知识库里还没有可练习的知识点。</p>}
        </section>

        {rubric && (
          <section className="border-b border-border py-6">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="flex items-center gap-2 text-sm font-semibold"><ShieldCheck className="size-4 text-primary" />本次评分标准</h2>
              <span className="text-xs text-muted-foreground">v{rubric.version_no}</span>
            </div>
            <div className="grid grid-cols-2 gap-x-4 gap-y-3 sm:gap-x-6">
              {rubric.criteria.map((criterion) => (
                <div key={criterion.dimension} className="border-l-2 border-primary/30 pl-3">
                  <div className="flex items-center justify-between gap-3 text-sm"><span className="font-medium">{criterion.label}</span><span className="text-xs text-muted-foreground">{criterion.weight}%</span></div>
                  <p className="mt-1 hidden text-xs leading-relaxed text-muted-foreground sm:block">{criterion.description}</p>
                </div>
              ))}
            </div>
          </section>
        )}

        {selectedPointID && !attempt?.confirmation && (
          <section className="border-b border-border py-5 text-center">
            <p className={`text-sm font-medium ${isRecording ? "text-[#C5682C]" : "text-muted-foreground"}`}>
              {isRecording
                ? `正在录音 ${Math.floor(elapsedMs / 60000)}:${String(Math.floor(elapsedMs / 1000) % 60).padStart(2, "0")}，再次点击麦克风结束`
                : `准备讲清楚「${selectedPoint?.title ?? "这个主题"}」`}
            </p>
            {busy === "uploading" && <p className="mt-4 flex items-center justify-center gap-2 text-sm text-primary"><LoaderCircle className="size-4 animate-spin" />上传并转写中</p>}
            {attempt?.status === "transcribing" && !busy && <p className="mt-4 text-sm text-muted-foreground">转写任务正在处理，页面会自动恢复结果。</p>}
            {attempt?.status === "failed" && <Button className="mt-4" variant="outline" onClick={startNewAttempt}><RefreshCw />重新开始</Button>}
          </section>
        )}

        {attempt?.status === "transcribed" && !attempt.confirmation && (
          <section className="border-b border-border py-6">
            <h2 className="mb-2 flex items-center gap-2 text-sm font-semibold"><FileText className="size-4 text-primary" />确认转写文本</h2>
            <p className="mb-3 text-xs text-muted-foreground">请修正错字或遗漏。只有你确认后的文本会进入评估。</p>
            <Textarea value={transcriptDraft} onChange={(event) => setTranscriptDraft(event.target.value)} className="min-h-44 resize-y leading-relaxed" maxLength={8000} />
            <div className="mt-3 flex items-center justify-between gap-3">
              <span className="text-xs text-muted-foreground">{attempt.active_audio_task?.stt_provider} · {transcriptDraft.length}/8000</span>
              <Button onClick={() => void confirmTranscript()} disabled={!transcriptDraft.trim() || Boolean(busy)}>{busy ? <LoaderCircle className="animate-spin" /> : <Check />}确认并评估</Button>
            </div>
          </section>
        )}

        {attempt?.confirmation && (
          <section className="border-b border-border py-6">
            <div className="mb-2 flex items-center gap-2 text-sm font-semibold text-primary"><Check className="size-4" />转写已确认</div>
            <blockquote className="border-l-2 border-primary/40 pl-4 text-sm leading-relaxed text-foreground">{attempt.confirmation.confirmed_transcript}</blockquote>
          </section>
        )}

        {(busy === "evaluating" || evaluation?.status === "evaluating") && (
          <section className="py-10 text-center"><LoaderCircle className="mx-auto mb-3 size-7 animate-spin text-primary" /><p className="text-sm font-medium">正在按 Rubric 核对输出与可信资料</p></section>
        )}

        {evaluation?.status === "failed" && (
          <section className="py-6"><p className="text-sm text-destructive">{evaluation.error_message || "评估失败"}</p><Button className="mt-3" variant="outline" onClick={() => void retryEvaluation()} disabled={Boolean(busy)}><RefreshCw />重试评估</Button></section>
        )}

        {evaluation?.status === "proposed" && evaluationDraft && (
          <section className="py-6">
            <div className="mb-4 flex items-start justify-between gap-4">
              <div><h2 className="text-base font-semibold">评估与证据候选</h2><p className="mt-1 text-xs text-muted-foreground">AI 只提出候选，确认前不会改变正式掌握状态。</p></div>
              <span className="whitespace-nowrap text-xs text-muted-foreground">{evaluation.prompt_version}</span>
            </div>
            <label className="text-xs font-medium text-muted-foreground">总体反馈</label>
            <Textarea className="mt-1 min-h-24" value={evaluationDraft.summary} disabled={evaluationResolved} onChange={(event) => setEvaluationDraft({ ...evaluationDraft, summary: event.target.value })} />
            <div className="mt-5 divide-y divide-border border-y border-border">
              {evaluationDraft.dimensions.map((dimension, index) => {
                const criterion = rubric?.criteria.find((item) => item.dimension === dimension.dimension);
                return (
                  <div key={dimension.dimension} className="py-4">
                    <div className="mb-2 flex items-center justify-between gap-4"><span className="text-sm font-semibold">{criterion?.label ?? dimension.dimension}</span><label className="flex items-center gap-2 text-xs text-muted-foreground">得分<input type="number" min={0} max={100} disabled={evaluationResolved} value={dimension.score} onChange={(event) => { const dimensions = [...evaluationDraft.dimensions]; dimensions[index] = { ...dimension, score: Math.max(0, Math.min(100, Number(event.target.value))) }; setEvaluationDraft({ ...evaluationDraft, dimensions }); }} className="h-8 w-16 rounded-md border border-input bg-input-background px-2 text-right text-sm text-foreground" /></label></div>
                    <Textarea value={dimension.feedback} disabled={evaluationResolved} onChange={(event) => { const dimensions = [...evaluationDraft.dimensions]; dimensions[index] = { ...dimension, feedback: event.target.value }; setEvaluationDraft({ ...evaluationDraft, dimensions }); }} className="min-h-20" />
                    {dimension.output_quotes.length > 0 && <p className="mt-2 text-xs text-muted-foreground">原话：“{dimension.output_quotes.join("” / “")}”</p>}
                    {dimension.source_refs.length > 0 && <p className="mt-1 text-xs text-muted-foreground">资料：{dimension.source_refs.join("、")}</p>}
                  </div>
                );
              })}
            </div>
            <div className="mt-5 border-l-2 border-primary pl-4">
              <label className="text-xs font-medium text-muted-foreground">掌握证据候选</label>
              <Textarea className="mt-1 min-h-20" disabled={evaluationResolved} value={evaluationDraft.evidence_candidate.claim} onChange={(event) => setEvaluationDraft({ ...evaluationDraft, evidence_candidate: { ...evaluationDraft.evidence_candidate, claim: event.target.value } })} />
              <Textarea className="mt-2 min-h-20" disabled={evaluationResolved} value={evaluationDraft.evidence_candidate.rationale} onChange={(event) => setEvaluationDraft({ ...evaluationDraft, evidence_candidate: { ...evaluationDraft.evidence_candidate, rationale: event.target.value } })} />
            </div>
            {evaluation.sources.length > 0 ? (
              <details className="mt-5 text-sm"><summary className="cursor-pointer font-medium">查看 {evaluation.sources.length} 条资料引用</summary><div className="mt-3 space-y-3">{evaluation.sources.map((source) => <div key={source.source_chunk_id} className="border-l border-border pl-3"><p className="text-xs font-medium">{source.ref} · {source.document_title} v{source.version_no}</p><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{source.content}</p></div>)}</div></details>
            ) : <p className="mt-4 text-xs text-amber-700">本次没有检索到已授权可信资料，评估只能核对表达结构，不能证明事实正确。</p>}

            {!evaluationResolved ? (
              <div className="mt-6">
                <Textarea placeholder="决策备注（可选）" value={decisionNote} onChange={(event) => setDecisionNote(event.target.value)} className="min-h-16" />
                <div className="mt-3 flex justify-end gap-2"><Button variant="outline" onClick={() => void decide("rejected")} disabled={Boolean(busy)}><X />拒绝候选</Button><Button onClick={() => void decide("confirmed")} disabled={Boolean(busy)}>{busy === "deciding" ? <LoaderCircle className="animate-spin" /> : <Check />}确认当前版本</Button></div>
              </div>
            ) : (
              <div className={`mt-6 flex items-center gap-2 text-sm font-medium ${evaluation.decision?.decision === "confirmed" ? "text-primary" : "text-muted-foreground"}`}>{evaluation.decision?.decision === "confirmed" ? <Check className="size-4" /> : <X className="size-4" />}{evaluation.decision?.decision === "confirmed" ? "已确认这条证据候选" : "已拒绝这条证据候选"}<Button className="ml-auto" variant="outline" size="sm" onClick={startNewAttempt}><RefreshCw />再练一次</Button></div>
            )}
          </section>
        )}

        {error && <div role="alert" className="mt-5 border-l-2 border-destructive pl-3 text-sm text-destructive">{error}</div>}
      </main>
    </div>
  );
});
