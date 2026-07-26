// KnowledgeBasePage 是知识库主界面，全部数据来自后端，不再使用任何演示数据。
//
// 三条边界在 UI 上必须看得见：
//   1. 上传成功 ≠ 资料可信：来源、类别、用途未确认前，资料只能停在「待确认」。
//   2. AI 只提出候选：候选必须由用户逐条确认、合并、归档或拒绝。
//   3. 确认候选 ≠ 掌握状态：知识点一律显示「暂无证据」，前端不提供任何掌握等级编辑入口。
//
// 所有状态都能通过重新拉取接口恢复，刷新页面不丢数据。
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { motion } from "motion/react";
import {
  AlertTriangle,
  Archive,
  Check,
  CheckCircle2,
  FileText,
  Inbox,
  Link2,
  Loader2,
  Pencil,
  RefreshCw,
  Search,
  Sparkles,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "./ui/dialog";
import { Input } from "./ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./ui/tabs";
import { Textarea } from "./ui/textarea";
import { ProfileButton } from "./ProfileButton";
import {
  APIError,
  CANDIDATE_STATUS_LABELS,
  CANDIDATE_TYPE_LABELS,
  CONTENT_ORIGIN_LABELS,
  CONTENT_ORIGIN_OPTIONS,
  DOCUMENT_KIND_LABELS,
  DOCUMENT_KIND_OPTIONS,
  DOCUMENT_PURPOSE_LABELS,
  DOCUMENT_PURPOSE_OPTIONS,
  DOCUMENT_STATUS_LABELS,
  MASTERY_UI_STATE_LABELS,
  archiveCandidate,
  confirmCandidate,
  confirmUsages,
  deleteDocument,
  extractCandidates,
  getVersionContent,
  labelOf,
  listCandidates,
  listChunks,
  listDocuments,
  listKnowledgePoints,
  listVersions,
  mergeCandidate,
  newIdempotencyKey,
  normalizePurposes,
  rejectCandidate,
  retryParse,
  setChunkRetrieval,
  updateCandidate,
  updateDocument,
  uploadDocument,
} from "../api/knowledge";
import type {
  ContentCandidate,
  ContentOrigin,
  DocumentKind,
  DocumentPurpose,
  DocumentStatus,
  DocumentVersion,
  KnowledgeDocument,
  KnowledgePoint,
  SourceChunk,
} from "../api/knowledge";

const STATUS_STYLES: Record<DocumentStatus, string> = {
  parsing: "bg-[#E8F0F7] text-[#456986]",
  pending_confirmation: "bg-[#FFF4D9] text-[#946B16]",
  ready: "bg-[#E4F1E8] text-[#2E6941]",
  failed: "bg-[#FBE7E5] text-[#9B3B32]",
  archived: "bg-secondary text-muted-foreground",
};

const CANDIDATE_STYLES: Record<string, string> = {
  knowledge_point: "bg-[#E8F0F7] text-[#456986]",
  plan_task: "bg-[#F7ECDD] text-[#8B5D28]",
  jd_requirement: "bg-[#F7ECDD] text-[#8B5D28]",
  personal_fact: "bg-[#E4F1E8] text-[#2E6941]",
  reference_only: "bg-secondary text-muted-foreground",
};

// CONFIRM_ACTION_LABELS 说明「确认」这一步实际会产生什么，避免用户以为确认即掌握。
const CONFIRM_ACTION_LABELS: Record<string, string> = {
  knowledge_point: "纳入知识库",
  plan_task: "待接入计划",
  jd_requirement: "待接入目标",
  personal_fact: "存为待核实事实",
  reference_only: "仅作参考资料",
};

const EXTRACTABLE_PURPOSES_BY_KIND: Record<DocumentKind, DocumentPurpose[]> = {
  learning_note: ["learn", "ai_retrieval"],
  learning_todo: ["generate_plan", "ai_retrieval"],
  technical_material: ["learn", "ai_retrieval"],
  target_jd: ["fact_reference", "ai_retrieval"],
  project_fact: ["fact_reference", "ai_retrieval"],
  interview_review: ["learn", "generate_plan", "fact_reference", "ai_retrieval"],
  other: ["learn", "generate_plan", "fact_reference", "ai_retrieval"],
};

function canExtractCandidates(document: KnowledgeDocument): boolean {
  return (
    document.status === "ready" &&
    document.purposes.some((purpose) => EXTRACTABLE_PURPOSES_BY_KIND[document.document_kind].includes(purpose))
  );
}

// PendingUpload 是上传中的乐观占位行。它不是后端数据，成功或失败后立即移除。
interface PendingUpload {
  key: string;
  filename: string;
  file: File;
  error?: string;
}

interface DocumentDraft {
  title: string;
  contentOrigin: ContentOrigin;
  documentKind: DocumentKind;
  purposes: DocumentPurpose[];
}

function formatTime(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatSize(bytes?: number): string {
  if (bytes === undefined || bytes === null) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

function messageOf(error: unknown, fallback: string): string {
  if (error instanceof APIError) return error.message;
  if (error instanceof Error) return error.message;
  return fallback;
}

interface KnowledgeBasePageProps {
  onOpenProfile: () => void;
}

export function KnowledgeBasePage({ onOpenProfile }: KnowledgeBasePageProps) {
  const [documents, setDocuments] = useState<KnowledgeDocument[]>([]);
  const [candidates, setCandidates] = useState<ContentCandidate[]>([]);
  const [knowledgePoints, setKnowledgePoints] = useState<KnowledgePoint[]>([]);

  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [notice, setNotice] = useState("");
  const [query, setQuery] = useState("");

  const [pendingUploads, setPendingUploads] = useState<PendingUpload[]>([]);
  const [busyDocumentID, setBusyDocumentID] = useState("");
  const [busyCandidateIDs, setBusyCandidateIDs] = useState<Set<string>>(() => new Set());
  const [busyChunkIDs, setBusyChunkIDs] = useState<Set<string>>(() => new Set());

  // 资料详情
  const [detailID, setDetailID] = useState<string | null>(null);
  const [draft, setDraft] = useState<DocumentDraft | null>(null);
  const [draftError, setDraftError] = useState("");
  const [versions, setVersions] = useState<DocumentVersion[]>([]);
  const [chunks, setChunks] = useState<SourceChunk[]>([]);
  const [rawText, setRawText] = useState<{ versionNo: number; text: string } | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);

  // 候选编辑
  const [editingCandidate, setEditingCandidate] = useState<ContentCandidate | null>(null);
  const [candidateDraft, setCandidateDraft] = useState({ title: "", summary: "", reason: "" });
  const [linkTargetID, setLinkTargetID] = useState("");
  const [mergeTargetID, setMergeTargetID] = useState("");
  const [candidateError, setCandidateError] = useState("");

  const fileInputRef = useRef<HTMLInputElement>(null);
  const reloadSequence = useRef(0);

  const detailDocument = useMemo(
    () => documents.find((item) => item.document_id === detailID) ?? null,
    [documents, detailID],
  );
  const detailCanExtract = detailDocument !== null && canExtractCandidates(detailDocument);

  // reload 是唯一的数据来源：所有写操作完成后都回到这里重新拉取，
  // 保证界面永远等于后端状态，刷新页面也不会丢。
  const reload = useCallback(async (showLoading = false) => {
    const requestSequence = ++reloadSequence.current;
    if (showLoading) {
      setLoading(true);
      setLoadError("");
    }
    try {
      const [docs, pending, points] = await Promise.all([
        listDocuments(),
        listCandidates({ status: "pending" }),
        listKnowledgePoints(),
      ]);
      if (requestSequence === reloadSequence.current) {
        setDocuments(docs);
        setCandidates(pending);
        setKnowledgePoints(points);
      }
    } catch (error) {
      if (requestSequence === reloadSequence.current) {
        setLoadError(messageOf(error, "加载知识库失败"));
      }
    } finally {
      if (requestSequence === reloadSequence.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload(true);
  }, [reload]);

  // 解析在上传请求内同步完成，但重试解析等操作仍可能留下 parsing 状态；
  // 只在确实存在 parsing 资料时轮询，避免无意义的定时请求。
  useEffect(() => {
    if (!documents.some((item) => item.status === "parsing")) return;
    const timer = window.setInterval(() => {
      void reload();
    }, 3000);
    return () => window.clearInterval(timer);
  }, [documents, reload]);

  const filteredDocuments = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    if (!normalized) return documents;
    return documents.filter((item) =>
      [item.title, item.current_version?.original_filename ?? ""]
        .join(" ")
        .toLowerCase()
        .includes(normalized),
    );
  }, [documents, query]);

  // -------------------------------------------------------------------------
  // 上传
  // -------------------------------------------------------------------------

  const submitUpload = async (upload: PendingUpload) => {
    setPendingUploads((current) =>
      current.map((item) => (item.key === upload.key ? { ...item, error: undefined } : item)),
    );
    setNotice("");
    try {
      const result = await uploadDocument(upload.file, upload.key);
      setPendingUploads((current) => current.filter((item) => item.key !== upload.key));
      if (result.idempotent_hit) {
        setNotice("同一次上传已完成，未重复创建版本。");
      } else if (result.duplicate_of_version_id) {
        setNotice("检测到内容完全相同的历史版本，已作为新版本保存，是否重复由你判断。");
      }
      await reload();
    } catch (error) {
      const message = messageOf(error, "上传资料失败");
      setPendingUploads((current) =>
        current.map((item) => (item.key === upload.key ? { ...item, error: message } : item)),
      );
    }
  };

  const handleUpload = (file: File) => {
    // 一次用户上传只生成一个幂等键；网络失败后的原地重试继续复用它。
    const upload = { key: newIdempotencyKey(), filename: file.name, file };
    setPendingUploads((current) => [...current, upload]);
    void submitUpload(upload);
  };

  const dismissUpload = (key: string) => {
    setPendingUploads((current) => current.filter((item) => item.key !== key));
  };

  const handleRetryParse = async (documentID: string) => {
    setBusyDocumentID(documentID);
    if (detailID === documentID) setDraftError("");
    else setNotice("");
    try {
      await retryParse(documentID);
      await reload();
    } catch (error) {
      const message = messageOf(error, "重新解析失败");
      if (detailID === documentID) setDraftError(message);
      else setNotice(message);
    } finally {
      setBusyDocumentID("");
    }
  };

  // -------------------------------------------------------------------------
  // 资料详情
  // -------------------------------------------------------------------------

  const openDetail = async (document: KnowledgeDocument) => {
    setDetailID(document.document_id);
    setDraftError("");
    setRawText(null);
    setDraft({
      title: document.title,
      contentOrigin: document.content_origin,
      documentKind: document.document_kind,
      purposes: document.purposes,
    });
    setDetailLoading(true);
    try {
      const [versionList, chunkList] = await Promise.all([
        listVersions(document.document_id),
        document.chunk_count > 0 ? listChunks(document.document_id) : Promise.resolve([]),
      ]);
      setVersions(versionList);
      setChunks(chunkList);
    } catch (error) {
      setDraftError(messageOf(error, "加载资料详情失败"));
    } finally {
      setDetailLoading(false);
    }
  };

  const closeDetail = () => {
    setDetailID(null);
    setDraft(null);
    setVersions([]);
    setChunks([]);
    setRawText(null);
    setDraftError("");
  };

  const togglePurpose = (purpose: DocumentPurpose) => {
    setDraft((current) => {
      if (!current) return current;
      const selected = current.purposes.includes(purpose);
      // 「仅归档」与其它用途互斥；后端也会再校验一次，这里只做即时反馈。
      if (purpose === "archive_only") {
        return { ...current, purposes: selected ? [] : ["archive_only"] };
      }
      const next = selected
        ? current.purposes.filter((item) => item !== purpose)
        : [...current.purposes.filter((item) => item !== "archive_only"), purpose];
      return { ...current, purposes: normalizePurposes(next) };
    });
  };

  // saveDetail 分两步提交：元数据与用途是后端两个不同的确认入口，语义不能混。
  const saveDetail = async () => {
    if (!detailDocument || !draft) return;
    const title = draft.title.trim();
    if (!title) {
      setDraftError("资料名称不能为空");
      return;
    }
    setBusyDocumentID(detailDocument.document_id);
    setDraftError("");
    try {
      await updateDocument(detailDocument.document_id, {
        title,
        content_origin: draft.contentOrigin,
        document_kind: draft.documentKind,
      });
      try {
        await confirmUsages(detailDocument.document_id, normalizePurposes(draft.purposes));
      } catch (error) {
        await reload();
        setDraftError(
          `来源、类别与标题已保存；用途保存失败：${messageOf(error, "请重试用途确认")}`,
        );
        return;
      }
      await reload();
      setNotice("已保存来源、类别与用途。");
      closeDetail();
    } catch (error) {
      setDraftError(messageOf(error, "保存来源、类别与标题失败"));
    } finally {
      setBusyDocumentID("");
    }
  };

  const handleViewRaw = async (version: DocumentVersion) => {
    if (!detailDocument) return;
    setDraftError("");
    try {
      const content = await getVersionContent(detailDocument.document_id, version.version_id);
      setRawText({ versionNo: content.version.version_no, text: content.raw_text });
    } catch (error) {
      setDraftError(messageOf(error, "查询资料原文失败"));
    }
  };

  const handleToggleChunk = async (chunk: SourceChunk) => {
    if (!detailDocument || busyChunkIDs.has(chunk.source_chunk_id)) return;
    setBusyChunkIDs((current) => new Set(current).add(chunk.source_chunk_id));
    setDraftError("");
    try {
      const updated = await setChunkRetrieval(
        detailDocument.document_id,
        chunk.source_chunk_id,
        !chunk.retrieval_enabled,
      );
      setChunks((current) =>
        current.map((item) =>
          item.source_chunk_id === updated.source_chunk_id ? updated : item,
        ),
      );
    } catch (error) {
      setDraftError(messageOf(error, "更新来源片段失败"));
    } finally {
      setBusyChunkIDs((current) => {
        const next = new Set(current);
        next.delete(chunk.source_chunk_id);
        return next;
      });
    }
  };

  const handleExtract = async () => {
    if (!detailDocument) return;
    setBusyDocumentID(detailDocument.document_id);
    setDraftError("");
    try {
      const result = await extractCandidates(detailDocument.document_id);
      const parts = [`新增 ${result.candidates.length} 条候选`];
      if (result.filtered > 0) parts.push(`丢弃越界 ${result.filtered} 条`);
      if (result.duplicated > 0) parts.push(`跳过重复 ${result.duplicated} 条`);
      setNotice(`${parts.join("，")}。候选仍需你逐条确认。`);
      await reload();
      closeDetail();
    } catch (error) {
      setDraftError(messageOf(error, "抽取候选内容失败"));
    } finally {
      setBusyDocumentID("");
    }
  };

  const handleDelete = async () => {
    if (!detailDocument) return;
    setBusyDocumentID(detailDocument.document_id);
    setDraftError("");
    try {
      await deleteDocument(detailDocument.document_id);
      await reload();
      setNotice("资料已删除。");
      closeDetail();
    } catch (error) {
      setDraftError(messageOf(error, "删除资料失败"));
    } finally {
      setBusyDocumentID("");
    }
  };

  // -------------------------------------------------------------------------
  // 候选处理
  // -------------------------------------------------------------------------

  const runCandidateAction = async (
    candidateID: string,
    action: () => Promise<unknown>,
    successNotice: string,
  ) => {
    if (busyCandidateIDs.has(candidateID)) return;
    setBusyCandidateIDs((current) => new Set(current).add(candidateID));
    setCandidateError("");
    try {
      await action();
      await reload();
      setNotice(successNotice);
      setEditingCandidate(null);
    } catch (error) {
      const message = messageOf(error, "处理候选内容失败");
      if (editingCandidate) setCandidateError(message);
      else setNotice(message);
      // 候选已被处理或已消失时刷新列表，避免界面停留在失效数据上。
      if (error instanceof APIError && (error.status === 409 || error.status === 404)) {
        await reload();
      }
    } finally {
      setBusyCandidateIDs((current) => {
        const next = new Set(current);
        next.delete(candidateID);
        return next;
      });
    }
  };

  const openCandidateEditor = (candidate: ContentCandidate) => {
    setEditingCandidate(candidate);
    setCandidateDraft({
      title: candidate.title,
      summary: candidate.summary ?? "",
      reason: candidate.reason ?? "",
    });
    setLinkTargetID("");
    setMergeTargetID("");
    setCandidateError("");
  };

  const mergeOptions = useMemo(() => {
    if (!editingCandidate) return [];
    return candidates.filter(
      (item) =>
        item.candidate_id !== editingCandidate.candidate_id &&
        item.candidate_type === editingCandidate.candidate_type,
    );
  }, [candidates, editingCandidate]);

  const pendingCount = candidates.length;

  // -------------------------------------------------------------------------
  // 渲染
  // -------------------------------------------------------------------------

  return (
    <motion.main
      initial={{ opacity: 0, y: 8 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.2, ease: "easeOut" }}
      className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background pb-16"
    >
      <header className="flex flex-shrink-0 items-start justify-between gap-4 px-5 pb-4 pt-8">
        <div>
          <h1 className="text-[20px] font-semibold text-foreground">知识库</h1>
          <p className="mt-0.5 text-[12px] text-muted-foreground">资料先确认用途，掌握只看真实证据</p>
        </div>
        <ProfileButton onClick={onOpenProfile} />
      </header>

      {notice && (
        <div className="mx-5 mb-3 flex items-start justify-between gap-2 border-l-2 border-[#C7944A] bg-[#FFF9EC] px-3 py-2">
          <p className="text-[10px] leading-4 text-muted-foreground">{notice}</p>
          <button type="button" onClick={() => setNotice("")} aria-label="关闭提示" className="text-muted-foreground">
            <X size={12} />
          </button>
        </div>
      )}

      <Tabs defaultValue="sources" className="min-h-0 flex-1 gap-0">
        <div className="px-5">
          <TabsList className="grid h-10 w-full grid-cols-3 rounded-lg bg-secondary p-1">
            <TabsTrigger value="sources" className="rounded-md text-[12px]">资料</TabsTrigger>
            <TabsTrigger value="candidates" className="rounded-md text-[12px]">
              待确认
              {pendingCount > 0 && <span className="ml-1 text-[9px] text-[#946B16]">{pendingCount}</span>}
            </TabsTrigger>
            <TabsTrigger value="mastery" className="rounded-md text-[12px]">掌握情况</TabsTrigger>
          </TabsList>
        </div>

        {/* ------------------------------ 资料 ------------------------------ */}
        <TabsContent value="sources" className="min-h-0 overflow-y-auto px-5 pb-24 pt-5">
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            className="flex h-11 w-full items-center justify-center gap-2 rounded-lg bg-primary text-[12px] font-semibold text-white transition-colors hover:bg-primary/90"
          >
            <Upload size={15} />
            上传 Markdown 资料
          </button>
          <input
            ref={fileInputRef}
            type="file"
            accept=".md,.markdown"
            className="hidden"
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) void handleUpload(file);
              event.target.value = "";
            }}
          />

          <div className="relative mt-4">
            <Search size={15} className="absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="搜索资料名称或文件名"
              className="h-10 bg-white pl-9 text-[12px]"
            />
          </div>

          <div className="mb-3 mt-5 flex items-center justify-between">
            <h2 className="text-[14px] font-semibold text-foreground">全部资料</h2>
            <span className="text-[11px] text-muted-foreground">{filteredDocuments.length} 份</span>
          </div>

          {/* 上传中：乐观占位行，不代表后端已经收下 */}
          <div className="space-y-2.5">
            {pendingUploads.map((upload) => (
              <article key={upload.key} className="rounded-lg border border-dashed border-border bg-white p-3.5">
                <div className="flex items-center gap-3">
                  <span className="flex size-9 flex-shrink-0 items-center justify-center rounded-lg bg-secondary text-primary">
                    {upload.error ? <AlertTriangle size={16} /> : <Loader2 size={16} className="animate-spin" />}
                  </span>
                  <div className="min-w-0 flex-1">
                    <h3 className="truncate text-[13px] font-semibold text-foreground">{upload.filename}</h3>
                    <p className="mt-0.5 text-[10px] leading-4 text-muted-foreground">
                      {upload.error ?? "上传中，完成后仍需确认来源与用途"}
                    </p>
                  </div>
                  {upload.error && (
                    <div className="flex flex-shrink-0 items-center gap-1">
                      <button
                        type="button"
                        onClick={() => void submitUpload(upload)}
                        title="使用同一幂等键重试"
                        className="flex h-8 items-center gap-1 rounded-md px-2 text-[10px] font-medium text-primary hover:bg-secondary"
                      >
                        <RefreshCw size={12} />
                        重试
                      </button>
                      <button
                        type="button"
                        onClick={() => dismissUpload(upload.key)}
                        aria-label="忽略上传失败"
                        className="flex size-8 items-center justify-center rounded-full text-muted-foreground hover:bg-secondary"
                      >
                        <X size={14} />
                      </button>
                    </div>
                  )}
                </div>
              </article>
            ))}
          </div>

          {loading ? (
            <div className="py-10 text-center">
              <Loader2 size={20} className="mx-auto animate-spin text-primary" />
              <p className="mt-2 text-[11px] text-muted-foreground">正在加载资料…</p>
            </div>
          ) : loadError ? (
            <div className="border-y border-border py-10 text-center">
              <AlertTriangle size={22} className="mx-auto text-[#9B3B32]" />
              <p className="mt-2 text-[13px] font-medium text-foreground">{loadError}</p>
              <button
                type="button"
                onClick={() => void reload(true)}
                className="mt-3 inline-flex h-8 items-center gap-1.5 rounded-md bg-primary px-3 text-[11px] font-semibold text-white"
              >
                <RefreshCw size={13} />
                重试
              </button>
            </div>
          ) : filteredDocuments.length === 0 ? (
            <div className="border-y border-border py-10 text-center">
              <Inbox size={22} className="mx-auto text-muted-foreground" />
              <p className="mt-2 text-[13px] font-medium text-foreground">
                {documents.length === 0 ? "还没有上传任何资料" : "没有匹配的资料"}
              </p>
              <p className="mt-1 text-[10px] leading-4 text-muted-foreground">
                上传 Markdown 后，先确认来源、类别和用途，再抽取候选内容。
              </p>
            </div>
          ) : (
            <div className="mt-2.5 space-y-2.5">
              {filteredDocuments.map((document) => (
                <article key={document.document_id} className="rounded-lg border border-border bg-white p-3.5">
                  <div className="flex items-start gap-3">
                    <span className="flex size-9 flex-shrink-0 items-center justify-center rounded-lg bg-secondary text-primary">
                      {document.status === "parsing" ? (
                        <Loader2 size={16} className="animate-spin" />
                      ) : document.status === "failed" ? (
                        <AlertTriangle size={16} />
                      ) : (
                        <FileText size={16} />
                      )}
                    </span>
                    <div className="min-w-0 flex-1">
                      <h3 className="truncate text-[13px] font-semibold text-foreground">{document.title}</h3>
                      <p className="mt-0.5 text-[10px] text-muted-foreground">
                        {document.current_version
                          ? `v${document.current_version.version_no} · ${formatSize(document.current_version.size_bytes)} · `
                          : ""}
                        {formatTime(document.updated_at)}
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() => void openDetail(document)}
                      aria-label={`查看资料：${document.title}`}
                      title="查看与确认"
                      className="flex size-8 flex-shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-secondary hover:text-primary"
                    >
                      <Pencil size={14} />
                    </button>
                  </div>

                  <div className="mt-3 flex items-center justify-between gap-3">
                    <span className={`rounded-md px-2 py-1 text-[10px] font-medium ${STATUS_STYLES[document.status]}`}>
                      {labelOf(DOCUMENT_STATUS_LABELS, document.status)}
                    </span>
                    <span className="text-[10px] text-muted-foreground">资料本身不计入掌握</span>
                  </div>

                  {document.status === "failed" && (
                    <div className="mt-3 flex items-center justify-between gap-3 border-l-2 border-[#9B3B32] bg-[#FDF1F0] px-3 py-2">
                      <p className="min-w-0 flex-1 text-[10px] leading-4 text-muted-foreground">
                        {document.parse_error || "解析失败，原文已保存，可直接重试"}
                      </p>
                      <button
                        type="button"
                        onClick={() => void handleRetryParse(document.document_id)}
                        disabled={busyDocumentID === document.document_id}
                        className="flex h-7 flex-shrink-0 items-center gap-1 rounded-md bg-primary px-2.5 text-[10px] font-semibold text-white disabled:opacity-40"
                      >
                        <RefreshCw size={11} />
                        重试解析
                      </button>
                    </div>
                  )}

                  <div className="mt-3 flex flex-wrap items-center gap-1.5 border-t border-border pt-3">
                    <span className="rounded-md border border-border px-2 py-1 text-[10px] text-muted-foreground">
                      来源：{labelOf(CONTENT_ORIGIN_LABELS, document.content_origin)}
                    </span>
                    <span className="rounded-md border border-border px-2 py-1 text-[10px] text-muted-foreground">
                      类别：{labelOf(DOCUMENT_KIND_LABELS, document.document_kind)}
                    </span>
                    <span className="rounded-md border border-border px-2 py-1 text-[10px] text-muted-foreground">
                      片段 {document.chunk_count}
                    </span>
                    {document.purposes.length > 0 ? (
                      document.purposes.map((purpose) => (
                        <span key={purpose} className="rounded-md bg-[#E8F0E8] px-2 py-1 text-[10px] text-[#2E6941]">
                          {labelOf(DOCUMENT_PURPOSE_LABELS, purpose)}
                        </span>
                      ))
                    ) : (
                      <span className="rounded-md bg-[#FFF4D9] px-2 py-1 text-[10px] text-[#946B16]">用途待确认</span>
                    )}
                  </div>
                </article>
              ))}
            </div>
          )}
        </TabsContent>

        {/* ----------------------------- 待确认 ----------------------------- */}
        <TabsContent value="candidates" className="min-h-0 overflow-y-auto px-5 pb-24 pt-5">
          <section className="border-l-2 border-[#C7944A] bg-[#FFF9EC] px-3.5 py-3" aria-label="候选内容说明">
            <div className="flex items-start gap-2.5">
              <Sparkles size={15} className="mt-0.5 flex-shrink-0 text-[#946B16]" />
              <div>
                <h2 className="text-[12px] font-semibold text-foreground">AI 只提出候选，不替你决定</h2>
                <p className="mt-1 text-[10px] leading-4 text-muted-foreground">
                  确认只决定候选流向哪条链路，不会产生任何掌握等级。
                </p>
              </div>
            </div>
          </section>

          <div className="mb-3 mt-5 flex items-center justify-between">
            <h2 className="text-[14px] font-semibold text-foreground">待处理内容</h2>
            <span className="text-[11px] text-muted-foreground">{candidates.length} 项</span>
          </div>

          {loading ? (
            <div className="py-10 text-center">
              <Loader2 size={20} className="mx-auto animate-spin text-primary" />
            </div>
          ) : candidates.length === 0 ? (
            <div className="border-y border-border py-10 text-center">
              <Check size={22} className="mx-auto text-primary" />
              <p className="mt-2 text-[13px] font-medium text-foreground">没有待确认的候选内容</p>
              <p className="mt-1 text-[10px] leading-4 text-muted-foreground">
                在资料详情里确认用途后点「抽取候选」，结果会出现在这里。
              </p>
            </div>
          ) : (
            <div className="space-y-2.5">
              {candidates.map((candidate) => (
                <article key={candidate.candidate_id} className="rounded-lg border border-border bg-white p-3.5">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span
                          className={`inline-flex rounded-md px-2 py-1 text-[10px] font-medium ${
                            CANDIDATE_STYLES[candidate.candidate_type] ?? "bg-secondary text-muted-foreground"
                          }`}
                        >
                          {labelOf(CANDIDATE_TYPE_LABELS, candidate.candidate_type)}
                        </span>
                        {candidate.source_content_origin === "ai_generated" && (
                          <span className="rounded-md bg-[#FFF4D9] px-2 py-1 text-[10px] text-[#946B16]">AI 整理，需核实</span>
                        )}
                      </div>
                      <h3 className="mt-2 text-[13px] font-semibold leading-5 text-foreground">{candidate.title}</h3>
                    </div>
                    <button
                      type="button"
                      onClick={() =>
                        void runCandidateAction(
                          candidate.candidate_id,
                          () => rejectCandidate(candidate.candidate_id),
                          "已拒绝该候选。",
                        )
                      }
                      disabled={busyCandidateIDs.has(candidate.candidate_id)}
                      aria-label={`拒绝候选：${candidate.title}`}
                      title="拒绝"
                      className="flex size-8 flex-shrink-0 items-center justify-center rounded-full text-muted-foreground hover:bg-secondary hover:text-foreground disabled:opacity-40"
                    >
                      <X size={14} />
                    </button>
                  </div>

                  {candidate.summary && (
                    <p className="mt-2 text-[11px] leading-5 text-foreground">{candidate.summary}</p>
                  )}

                  {candidate.sources.length > 0 && (
                    <div className="mt-3 space-y-1.5 border-l-2 border-border pl-3">
                      <p className="text-[10px] font-medium text-muted-foreground">来源片段</p>
                      {candidate.sources.map((source) => (
                        <p key={source.source_chunk_id} className="text-[11px] leading-5 text-foreground">
                          “{source.evidence_quote || "（未提供引用原文）"}”
                        </p>
                      ))}
                    </div>
                  )}

                  {candidate.reason && (
                    <p className="mt-3 text-[10px] leading-4 text-muted-foreground">{candidate.reason}</p>
                  )}

                  <div className="mt-3 flex flex-wrap items-center justify-end gap-2 border-t border-border pt-3">
                    <button
                      type="button"
                      onClick={() => openCandidateEditor(candidate)}
                      className="flex h-8 items-center gap-1.5 rounded-md px-2.5 text-[11px] font-medium text-muted-foreground hover:bg-secondary"
                    >
                      <Pencil size={13} />
                      修改 / 关联
                    </button>
                    <button
                      type="button"
                      onClick={() =>
                        void runCandidateAction(
                          candidate.candidate_id,
                          () => archiveCandidate(candidate.candidate_id),
                          "已归档该候选。",
                        )
                      }
                      disabled={busyCandidateIDs.has(candidate.candidate_id)}
                      className="flex h-8 items-center gap-1.5 rounded-md px-2.5 text-[11px] font-medium text-muted-foreground hover:bg-secondary disabled:opacity-40"
                    >
                      <Archive size={13} />
                      仅归档
                    </button>
                    <button
                      type="button"
                      onClick={() =>
                        void runCandidateAction(
                          candidate.candidate_id,
                          () => confirmCandidate(candidate.candidate_id),
                          "已确认该候选。掌握状态不会因此改变。",
                        )
                      }
                      disabled={busyCandidateIDs.has(candidate.candidate_id)}
                      className="flex h-8 items-center gap-1.5 rounded-md bg-primary px-3 text-[11px] font-semibold text-white hover:bg-primary/90 disabled:opacity-40"
                    >
                      <Check size={13} />
                      {CONFIRM_ACTION_LABELS[candidate.candidate_type] ?? "确认"}
                    </button>
                  </div>
                </article>
              ))}
            </div>
          )}
        </TabsContent>

        {/* ---------------------------- 掌握情况 ---------------------------- */}
        <TabsContent value="mastery" className="min-h-0 overflow-y-auto px-5 pb-24 pt-5">
          <section className="border-l-2 border-[#456986] bg-[#F2F6FA] px-3.5 py-3" aria-label="掌握状态说明">
            <p className="text-[12px] font-semibold text-foreground">本阶段没有任何掌握证据</p>
            <p className="mt-1 text-[10px] leading-4 text-muted-foreground">
              上传、解析和候选确认都不会产生掌握等级。知识点只表示「已在追踪」，
              等级要等真实输出证据（讲清、独立实现、完整验证）接入后才会出现。
            </p>
          </section>

          <div className="mb-3 mt-5 flex items-center justify-between">
            <h2 className="text-[14px] font-semibold text-foreground">知识点</h2>
            <span className="text-[11px] text-muted-foreground">{knowledgePoints.length} 项</span>
          </div>

          {loading ? (
            <div className="py-10 text-center">
              <Loader2 size={20} className="mx-auto animate-spin text-primary" />
            </div>
          ) : knowledgePoints.length === 0 ? (
            <div className="border-y border-border py-10 text-center">
              <Inbox size={22} className="mx-auto text-muted-foreground" />
              <p className="mt-2 text-[13px] font-medium text-foreground">还没有知识点</p>
              <p className="mt-1 text-[10px] text-muted-foreground">确认「知识点」类型的候选后会出现在这里。</p>
            </div>
          ) : (
            <div className="space-y-2.5">
              {knowledgePoints.map((point) => (
                <article key={point.knowledge_point_id} className="rounded-lg border border-border bg-white p-3.5">
                  <div className="flex items-start gap-3">
                    <span className="flex size-9 flex-shrink-0 items-center justify-center rounded-lg bg-secondary text-primary">
                      <CheckCircle2 size={16} />
                    </span>
                    <div className="min-w-0 flex-1">
                      <h3 className="text-[13px] font-semibold text-foreground">{point.title}</h3>
                      <p className="mt-0.5 text-[10px] text-muted-foreground">加入于 {formatTime(point.created_at)}</p>
                    </div>
                  </div>
                  <div className="mt-3 flex items-center justify-between gap-3">
                    <span className="rounded-md bg-secondary px-2 py-1 text-[10px] font-medium text-muted-foreground">
                      {labelOf(MASTERY_UI_STATE_LABELS, point.mastery_ui_state)}
                    </span>
                    <span className="text-[10px] text-muted-foreground">等待第一条输出证据</span>
                  </div>
                  {point.description && (
                    <p className="mt-3 border-t border-border pt-3 text-[11px] leading-5 text-muted-foreground">
                      {point.description}
                    </p>
                  )}
                </article>
              ))}
            </div>
          )}
        </TabsContent>
      </Tabs>

      {/* --------------------------- 资料详情弹窗 --------------------------- */}
      <Dialog open={detailID !== null} onOpenChange={(open) => !open && closeDetail()}>
        <DialogContent className="z-[70] max-h-[calc(100vh-2rem)] max-w-[360px] gap-5 overflow-y-auto rounded-2xl border-border bg-white p-5">
          <DialogHeader className="text-left">
            <DialogTitle>资料详情</DialogTitle>
            <DialogDescription>来源、类别和用途必须由你确认；确认用途不代表已经学习或掌握。</DialogDescription>
          </DialogHeader>

          {draftError && (
            <p className="border-l-2 border-[#9B3B32] bg-[#FDF1F0] px-3 py-2 text-[10px] leading-4 text-[#9B3B32]">
              {draftError}
            </p>
          )}

          {draft && detailDocument && (
            <div className="space-y-4">
              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">名称</span>
                <Input
                  value={draft.title}
                  onChange={(event) => setDraft({ ...draft, title: event.target.value })}
                  placeholder="资料名称"
                />
              </label>

              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">内容来源</span>
                <Select
                  value={draft.contentOrigin}
                  onValueChange={(value: ContentOrigin) => setDraft({ ...draft, contentOrigin: value })}
                >
                  <SelectTrigger aria-label="内容来源"><SelectValue /></SelectTrigger>
                  <SelectContent className="z-[80]">
                    {CONTENT_ORIGIN_OPTIONS.map((origin) => (
                      <SelectItem key={origin} value={origin}>{CONTENT_ORIGIN_LABELS[origin]}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </label>

              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">内容类别</span>
                <Select
                  value={draft.documentKind}
                  onValueChange={(value: DocumentKind) => setDraft({ ...draft, documentKind: value })}
                >
                  <SelectTrigger aria-label="内容类别"><SelectValue /></SelectTrigger>
                  <SelectContent className="z-[80]">
                    {DOCUMENT_KIND_OPTIONS.map((kind) => (
                      <SelectItem key={kind} value={kind}>{DOCUMENT_KIND_LABELS[kind]}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </label>

              <fieldset>
                <legend className="mb-1.5 text-[12px] font-medium text-foreground">资料用途（可多选）</legend>
                <div className="grid grid-cols-2 gap-2">
                  {DOCUMENT_PURPOSE_OPTIONS.map((purpose) => (
                    <label
                      key={purpose}
                      className="flex cursor-pointer items-center gap-2 rounded-md border border-border px-2.5 py-2 text-[11px] text-foreground"
                    >
                      <input
                        type="checkbox"
                        checked={draft.purposes.includes(purpose)}
                        onChange={() => togglePurpose(purpose)}
                        className="size-3.5 accent-[#28573A]"
                      />
                      {DOCUMENT_PURPOSE_LABELS[purpose]}
                    </label>
                  ))}
                </div>
                <p className="mt-1.5 text-[10px] leading-4 text-muted-foreground">
                  「仅归档」与其它用途互斥；「供 AI 检索」只表示可作为回答来源，不代表你已经学习或掌握。
                </p>
              </fieldset>

              {/* 版本历史：版本只新增不覆盖，旧版本永远可追溯 */}
              <section>
                <h3 className="mb-1.5 text-[12px] font-medium text-foreground">版本历史</h3>
                {detailLoading ? (
                  <Loader2 size={14} className="animate-spin text-primary" />
                ) : versions.length === 0 ? (
                  <p className="text-[10px] text-muted-foreground">暂无版本记录。</p>
                ) : (
                  <ul className="space-y-1.5">
                    {versions.map((version) => (
                      <li
                        key={version.version_id}
                        className="flex items-center justify-between gap-2 rounded-md border border-border px-2.5 py-2"
                      >
                        <div className="min-w-0">
                          <p className="truncate text-[11px] text-foreground">
                            v{version.version_no} · {version.original_filename}
                          </p>
                          <p className="text-[10px] text-muted-foreground">
                            {formatSize(version.size_bytes)} · {formatTime(version.created_at)}
                          </p>
                        </div>
                        <button
                          type="button"
                          onClick={() => void handleViewRaw(version)}
                          className="flex h-7 flex-shrink-0 items-center gap-1 rounded-md px-2 text-[10px] font-medium text-primary hover:bg-secondary"
                        >
                          <Link2 size={11} />
                          查看原文
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </section>

              {rawText && (
                <section>
                  <h3 className="mb-1.5 text-[12px] font-medium text-foreground">v{rawText.versionNo} 原文</h3>
                  <Textarea readOnly value={rawText.text} className="min-h-40 font-mono text-[10px]" />
                </section>
              )}

              {/* 来源片段：候选内容与 AI 回答都要能回到这里 */}
              <section>
                <h3 className="mb-1.5 text-[12px] font-medium text-foreground">来源片段（{chunks.length}）</h3>
                {detailLoading ? (
                  <Loader2 size={14} className="animate-spin text-primary" />
                ) : chunks.length === 0 ? (
                  <p className="text-[10px] text-muted-foreground">解析成功后会在这里列出可引用的片段。</p>
                ) : (
                  <ul className="space-y-1.5">
                    {chunks.map((chunk) => (
                      <li key={chunk.source_chunk_id} className="rounded-md border border-border px-2.5 py-2">
                        <p className="text-[10px] text-muted-foreground">
                          #{chunk.ordinal + 1} {chunk.heading_path.join(" / ") || "（无标题）"}
                        </p>
                        <p className="mt-1 line-clamp-3 text-[11px] leading-5 text-foreground">{chunk.content}</p>
                        <label className="mt-2 flex cursor-pointer items-center gap-2 text-[10px] leading-4 text-muted-foreground">
                          <input
                            type="checkbox"
                            checked={chunk.retrieval_enabled}
                            disabled={
                              !detailDocument.purposes.includes("ai_retrieval") ||
                              busyChunkIDs.has(chunk.source_chunk_id)
                            }
                            onChange={() => void handleToggleChunk(chunk)}
                            className="size-3.5 accent-[#28573A]"
                          />
                          纳入 AI 检索
                          {!detailDocument.purposes.includes("ai_retrieval") && "（需先确认「供 AI 检索」用途）"}
                        </label>
                      </li>
                    ))}
                  </ul>
                )}
              </section>

              <div className="flex flex-wrap items-center gap-2 border-t border-border pt-4">
                <button
                  type="button"
                  onClick={() => void handleExtract()}
                  disabled={
                    busyDocumentID === detailDocument.document_id || !detailCanExtract
                  }
                  title={detailCanExtract ? "抽取待确认候选" : "当前类别与用途不能产生候选"}
                  className="flex h-8 items-center gap-1.5 rounded-md border border-border px-2.5 text-[11px] font-medium text-foreground hover:bg-secondary disabled:opacity-40"
                >
                  <Sparkles size={13} />
                  抽取候选
                </button>
                {detailDocument.status === "failed" && (
                  <button
                    type="button"
                    onClick={() => void handleRetryParse(detailDocument.document_id)}
                    disabled={busyDocumentID === detailDocument.document_id}
                    className="flex h-8 items-center gap-1.5 rounded-md border border-border px-2.5 text-[11px] font-medium text-foreground hover:bg-secondary disabled:opacity-40"
                  >
                    <RefreshCw size={13} />
                    重试解析
                  </button>
                )}
                <button
                  type="button"
                  onClick={() => void handleDelete()}
                  disabled={busyDocumentID === detailDocument.document_id}
                  className="flex h-8 items-center gap-1.5 rounded-md px-2.5 text-[11px] font-medium text-[#9B3B32] hover:bg-[#FDF1F0] disabled:opacity-40"
                >
                  <Trash2 size={13} />
                  删除资料
                </button>
              </div>
            </div>
          )}

          <DialogFooter className="flex-row justify-end">
            <button
              type="button"
              onClick={closeDetail}
              className="rounded-lg px-4 py-2 text-[13px] font-medium text-muted-foreground hover:bg-secondary"
            >
              取消
            </button>
            <button
              type="button"
              onClick={() => void saveDetail()}
              disabled={!draft?.title.trim() || busyDocumentID !== ""}
              className="rounded-lg bg-primary px-4 py-2 text-[13px] font-semibold text-white hover:bg-primary/90 disabled:opacity-40"
            >
              保存确认
            </button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* --------------------------- 候选处理弹窗 --------------------------- */}
      <Dialog open={editingCandidate !== null} onOpenChange={(open) => !open && setEditingCandidate(null)}>
        <DialogContent className="z-[70] max-h-[calc(100vh-2rem)] max-w-[360px] gap-5 overflow-y-auto rounded-2xl border-border bg-white p-5">
          <DialogHeader className="text-left">
            <DialogTitle>处理候选内容</DialogTitle>
            <DialogDescription>
              修改后仍然是候选；只有「确认」才会让它进入正式链路，且不产生掌握等级。
            </DialogDescription>
          </DialogHeader>

          {candidateError && (
            <p className="border-l-2 border-[#9B3B32] bg-[#FDF1F0] px-3 py-2 text-[10px] leading-4 text-[#9B3B32]">
              {candidateError}
            </p>
          )}

          {editingCandidate && (
            <div className="space-y-4">
              <p className="text-[10px] text-muted-foreground">
                类型：{labelOf(CANDIDATE_TYPE_LABELS, editingCandidate.candidate_type)} · 状态：
                {labelOf(CANDIDATE_STATUS_LABELS, editingCandidate.status)}
              </p>

              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">标题</span>
                <Input
                  value={candidateDraft.title}
                  onChange={(event) => setCandidateDraft({ ...candidateDraft, title: event.target.value })}
                />
              </label>
              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">摘要</span>
                <Textarea
                  value={candidateDraft.summary}
                  onChange={(event) => setCandidateDraft({ ...candidateDraft, summary: event.target.value })}
                  className="min-h-20"
                />
              </label>
              <label className="block">
                <span className="mb-1.5 block text-[12px] font-medium text-foreground">理由</span>
                <Textarea
                  value={candidateDraft.reason}
                  onChange={(event) => setCandidateDraft({ ...candidateDraft, reason: event.target.value })}
                  className="min-h-16"
                />
              </label>

              <button
                type="button"
                onClick={() =>
                  void runCandidateAction(
                    editingCandidate.candidate_id,
                    () =>
                      updateCandidate(editingCandidate.candidate_id, {
                        title: candidateDraft.title.trim(),
                        summary: candidateDraft.summary.trim(),
                        reason: candidateDraft.reason.trim(),
                      }),
                    "已修改候选内容，它仍在待确认列表里。",
                  )
                }
                disabled={
                  !candidateDraft.title.trim() ||
                  (editingCandidate !== null && busyCandidateIDs.has(editingCandidate.candidate_id))
                }
                className="h-9 w-full rounded-md border border-border text-[12px] font-medium text-foreground hover:bg-secondary disabled:opacity-40"
              >
                只保存修改
              </button>

              {editingCandidate.candidate_type === "knowledge_point" && knowledgePoints.length > 0 && (
                <div>
                  <span className="mb-1.5 block text-[12px] font-medium text-foreground">关联已有知识点</span>
                  <Select value={linkTargetID} onValueChange={setLinkTargetID}>
                    <SelectTrigger aria-label="关联已有知识点">
                      <SelectValue placeholder="选择一个知识点" />
                    </SelectTrigger>
                    <SelectContent className="z-[80]">
                      {knowledgePoints.map((point) => (
                        <SelectItem key={point.knowledge_point_id} value={point.knowledge_point_id}>
                          {point.title}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <button
                    type="button"
                    onClick={() =>
                      void runCandidateAction(
                        editingCandidate.candidate_id,
                        () => confirmCandidate(editingCandidate.candidate_id, { knowledge_point_id: linkTargetID }),
                        "已关联到已有知识点。",
                      )
                    }
                    disabled={
                      !linkTargetID ||
                      (editingCandidate !== null && busyCandidateIDs.has(editingCandidate.candidate_id))
                    }
                    className="mt-2 h-9 w-full rounded-md border border-border text-[12px] font-medium text-foreground hover:bg-secondary disabled:opacity-40"
                  >
                    确认并关联
                  </button>
                </div>
              )}

              {mergeOptions.length > 0 && (
                <div>
                  <span className="mb-1.5 block text-[12px] font-medium text-foreground">合并到其它候选</span>
                  <Select value={mergeTargetID} onValueChange={setMergeTargetID}>
                    <SelectTrigger aria-label="合并到其它候选">
                      <SelectValue placeholder="选择同类型候选" />
                    </SelectTrigger>
                    <SelectContent className="z-[80]">
                      {mergeOptions.map((item) => (
                        <SelectItem key={item.candidate_id} value={item.candidate_id}>
                          {item.title}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <button
                    type="button"
                    onClick={() =>
                      void runCandidateAction(
                        editingCandidate.candidate_id,
                        () => mergeCandidate(editingCandidate.candidate_id, mergeTargetID),
                        "已合并到目标候选。",
                      )
                    }
                    disabled={
                      !mergeTargetID ||
                      (editingCandidate !== null && busyCandidateIDs.has(editingCandidate.candidate_id))
                    }
                    className="mt-2 h-9 w-full rounded-md border border-border text-[12px] font-medium text-foreground hover:bg-secondary disabled:opacity-40"
                  >
                    确认合并
                  </button>
                </div>
              )}
            </div>
          )}

          <DialogFooter className="flex-row justify-end">
            <button
              type="button"
              onClick={() => setEditingCandidate(null)}
              className="rounded-lg px-4 py-2 text-[13px] font-medium text-muted-foreground hover:bg-secondary"
            >
              关闭
            </button>
            <button
              type="button"
              onClick={() => {
                if (!editingCandidate) return;
                void runCandidateAction(
                  editingCandidate.candidate_id,
                  () =>
                    confirmCandidate(editingCandidate.candidate_id, {
                      payload: {
                        title: candidateDraft.title.trim(),
                        summary: candidateDraft.summary.trim(),
                        reason: candidateDraft.reason.trim(),
                      },
                    }),
                  "已确认该候选。掌握状态不会因此改变。",
                );
              }}
              disabled={
                !candidateDraft.title.trim() ||
                (editingCandidate !== null && busyCandidateIDs.has(editingCandidate.candidate_id))
              }
              className="rounded-lg bg-primary px-4 py-2 text-[13px] font-semibold text-white hover:bg-primary/90 disabled:opacity-40"
            >
              修改后确认
            </button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.main>
  );
}
