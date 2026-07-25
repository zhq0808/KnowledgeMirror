// knowledge.ts 封装知识库（资料 / 候选内容 / 知识点）相关的后端接口。
//
// 三条不可让步的产品规则，前端必须原样遵守：
//   1. 上传成功 ≠ 资料可信：来源、类别、用途都要用户显式确认后才生效。
//   2. 确认候选 ≠ 掌握状态：本阶段后端不产生任何掌握等级，前端也不得自己造。
//   3. 枚举文案只做「英文枚举 → 中文展示」的映射，不做语义加工。
//
// 后端统一响应格式：{ code, message, data, trace_id }，code === 0 表示成功。

const basePath = "/api/v1";

interface APIResponse {
  code: number;
  message: string;
  trace_id?: string;
}

// APIError 保留 HTTP 状态码，方便调用方区分 404 / 409 / 502 / 503 等语义。
export class APIError extends Error {
  readonly status: number;
  readonly code: number;

  constructor(message: string, status: number, code: number) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.code = code;
  }
}

// request 统一拆包响应信封。data 允许为空（如删除接口），由调用方决定是否使用。
async function request<T>(
  path: string,
  init: RequestInit,
  fallbackMessage: string,
): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`${basePath}${path}`, { credentials: "include", ...init });
  } catch {
    // 网络层失败拿不到响应体，用 0 表示「请求没到达后端」。
    throw new APIError("网络异常，请检查连接后重试", 0, -1);
  }

  let body: (APIResponse & { data?: T }) | null = null;
  try {
    body = (await res.json()) as APIResponse & { data?: T };
  } catch {
    body = null;
  }

  if (!res.ok || !body || body.code !== 0) {
    throw new APIError(
      body?.message || fallbackMessage,
      res.status,
      body?.code ?? -1,
    );
  }
  return body.data as T;
}

function getJSON<T>(path: string, fallbackMessage: string): Promise<T> {
  return request<T>(path, { method: "GET" }, fallbackMessage);
}

function sendJSON<T>(
  method: "POST" | "PATCH" | "PUT" | "DELETE",
  path: string,
  payload: unknown,
  fallbackMessage: string,
): Promise<T> {
  const init: RequestInit = { method };
  if (payload !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(payload);
  }
  return request<T>(path, init, fallbackMessage);
}

// ---------------------------------------------------------------------------
// 枚举与中文文案
// ---------------------------------------------------------------------------

export type ContentOrigin =
  | "user_authored"
  | "ai_generated"
  | "external"
  | "pending_confirmation";

export type DocumentKind =
  | "learning_note"
  | "learning_todo"
  | "technical_material"
  | "target_jd"
  | "project_fact"
  | "interview_review"
  | "other";

export type DocumentPurpose =
  | "learn"
  | "ai_retrieval"
  | "generate_plan"
  | "fact_reference"
  | "archive_only";

export type DocumentStatus =
  | "parsing"
  | "pending_confirmation"
  | "ready"
  | "failed"
  | "archived";

export type CandidateType =
  | "knowledge_point"
  | "plan_task"
  | "jd_requirement"
  | "personal_fact"
  | "reference_only";

export type CandidateStatus =
  | "pending"
  | "confirmed"
  | "linked"
  | "merged"
  | "archived"
  | "rejected";

export type CandidateOutcome =
  | "knowledge_point_created"
  | "knowledge_point_linked"
  | "plan_task_pending_intake"
  | "jd_requirement_pending_intake"
  | "unverified_fact"
  | "reference_only"
  | "merged"
  | "archived"
  | "rejected";

export type TrustLevel = "unverified" | "user_confirmed" | "trusted";

export const CONTENT_ORIGIN_LABELS: Record<ContentOrigin, string> = {
  user_authored: "我自己写的",
  ai_generated: "AI 整理",
  external: "外部资料",
  pending_confirmation: "来源待确认",
};

export const DOCUMENT_KIND_LABELS: Record<DocumentKind, string> = {
  learning_note: "学习笔记",
  learning_todo: "学习 Todo",
  technical_material: "技术资料",
  target_jd: "目标 JD",
  project_fact: "项目事实",
  interview_review: "面试复盘",
  other: "其他",
};

export const DOCUMENT_PURPOSE_LABELS: Record<DocumentPurpose, string> = {
  learn: "供我学习",
  ai_retrieval: "供 AI 检索",
  generate_plan: "生成计划",
  fact_reference: "事实参考",
  archive_only: "仅归档",
};

export const DOCUMENT_STATUS_LABELS: Record<DocumentStatus, string> = {
  parsing: "解析中",
  pending_confirmation: "待确认",
  ready: "可用",
  failed: "解析失败",
  archived: "仅归档",
};

export const CANDIDATE_TYPE_LABELS: Record<CandidateType, string> = {
  knowledge_point: "知识点",
  plan_task: "计划任务",
  jd_requirement: "JD 要求",
  personal_fact: "待核实事实",
  reference_only: "参考资料",
};

export const CANDIDATE_STATUS_LABELS: Record<CandidateStatus, string> = {
  pending: "待确认",
  confirmed: "已确认",
  linked: "已关联",
  merged: "已合并",
  archived: "已归档",
  rejected: "已拒绝",
};

export const CANDIDATE_OUTCOME_LABELS: Record<CandidateOutcome, string> = {
  knowledge_point_created: "已加入知识库",
  knowledge_point_linked: "已关联已有知识点",
  plan_task_pending_intake: "待接入学习计划",
  jd_requirement_pending_intake: "待接入目标要求",
  unverified_fact: "待核实事实",
  reference_only: "仅作参考资料",
  merged: "已合并到其它候选",
  archived: "已归档",
  rejected: "已拒绝",
};

export const TRUST_LEVEL_LABELS: Record<TrustLevel, string> = {
  unverified: "未核实",
  user_confirmed: "我已确认",
  trusted: "可信",
};

// MASTERY_UI_STATE_LABELS 只有一个取值：本阶段后端不产生任何掌握证据。
// no_evidence 是空状态，不是掌握等级——不要在这里补 exposed / verified 之类的值。
export const MASTERY_UI_STATE_LABELS: Record<string, string> = {
  no_evidence: "暂无证据",
};

export function labelOf<T extends string>(
  labels: Record<string, string>,
  value: T | string | undefined,
): string {
  if (!value) return "未知";
  return labels[value] ?? value;
}

export const CONTENT_ORIGIN_OPTIONS = Object.keys(
  CONTENT_ORIGIN_LABELS,
) as ContentOrigin[];
export const DOCUMENT_KIND_OPTIONS = Object.keys(
  DOCUMENT_KIND_LABELS,
) as DocumentKind[];
export const DOCUMENT_PURPOSE_OPTIONS = Object.keys(
  DOCUMENT_PURPOSE_LABELS,
) as DocumentPurpose[];

// ---------------------------------------------------------------------------
// 资料
// ---------------------------------------------------------------------------

export interface DocumentVersion {
  version_id: string;
  version_no: number;
  original_filename: string;
  mime_type: string;
  size_bytes: number;
  sha256: string;
  parser_version: string;
  created_at: string;
}

export interface KnowledgeDocument {
  document_id: string;
  title: string;
  content_origin: ContentOrigin;
  document_kind: DocumentKind;
  status: DocumentStatus;
  parse_error?: string;
  parsed_at?: string;
  created_at: string;
  updated_at: string;
  current_version?: DocumentVersion;
  purposes: DocumentPurpose[];
  chunk_count: number;
}

export interface SourceChunk {
  source_chunk_id: string;
  version_id: string;
  ordinal: number;
  heading_path: string[];
  content: string;
  start_offset: number;
  end_offset: number;
  trust_level: TrustLevel;
  retrieval_enabled: boolean;
}

export interface UploadDocumentResult {
  document: KnowledgeDocument;
  idempotent_hit: boolean;
  duplicate_of_version_id?: string;
}

export interface VersionContent {
  version: DocumentVersion;
  raw_text: string;
}

export function listDocuments(): Promise<KnowledgeDocument[]> {
  return getJSON<KnowledgeDocument[]>("/documents", "查询资料列表失败");
}

export function getDocument(documentID: string): Promise<KnowledgeDocument> {
  return getJSON<KnowledgeDocument>(
    `/documents/${documentID}`,
    "查询资料失败",
  );
}

// uploadDocument 上传 Markdown。idempotencyKey 由调用方生成并在重试时保持不变，
// 保证「网络超时后重试」不会产生第二个版本。
export async function uploadDocument(
  file: File,
  idempotencyKey: string,
  options?: { title?: string; documentID?: string },
): Promise<UploadDocumentResult> {
  const form = new FormData();
  form.append("file", file);
  if (options?.title) form.append("title", options.title);
  if (options?.documentID) form.append("document_id", options.documentID);

  return request<UploadDocumentResult>(
    "/documents",
    { method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: form },
    "上传资料失败",
  );
}

export function updateDocument(
  documentID: string,
  payload: {
    title?: string;
    content_origin?: ContentOrigin;
    document_kind?: DocumentKind;
  },
): Promise<KnowledgeDocument> {
  return sendJSON<KnowledgeDocument>(
    "PATCH",
    `/documents/${documentID}`,
    payload,
    "修改资料失败",
  );
}

// confirmUsages 整体覆盖用途集合。传空数组表示撤回全部用途。
export function confirmUsages(
  documentID: string,
  purposes: DocumentPurpose[],
): Promise<KnowledgeDocument> {
  return sendJSON<KnowledgeDocument>(
    "PUT",
    `/documents/${documentID}/usages`,
    { purposes },
    "确认资料用途失败",
  );
}

export function deleteDocument(documentID: string): Promise<void> {
  return sendJSON<void>(
    "DELETE",
    `/documents/${documentID}`,
    undefined,
    "删除资料失败",
  );
}

export function retryParse(documentID: string): Promise<KnowledgeDocument> {
  return sendJSON<KnowledgeDocument>(
    "POST",
    `/documents/${documentID}/parse/retry`,
    undefined,
    "重新解析资料失败",
  );
}

export function listVersions(documentID: string): Promise<DocumentVersion[]> {
  return getJSON<DocumentVersion[]>(
    `/documents/${documentID}/versions`,
    "查询资料版本失败",
  );
}

export function getVersionContent(
  documentID: string,
  versionID: string,
): Promise<VersionContent> {
  return getJSON<VersionContent>(
    `/documents/${documentID}/versions/${versionID}`,
    "查询资料原文失败",
  );
}

export function listChunks(
  documentID: string,
  versionID?: string,
): Promise<SourceChunk[]> {
  const query = versionID ? `?version_id=${encodeURIComponent(versionID)}` : "";
  return getJSON<SourceChunk[]>(
    `/documents/${documentID}/chunks${query}`,
    "查询来源片段失败",
  );
}

export function setChunkRetrieval(
  documentID: string,
  chunkID: string,
  enabled: boolean,
): Promise<SourceChunk> {
  return sendJSON<SourceChunk>(
    "PATCH",
    `/documents/${documentID}/chunks/${chunkID}`,
    { retrieval_enabled: enabled },
    "更新来源片段失败",
  );
}

// ---------------------------------------------------------------------------
// 候选内容
// ---------------------------------------------------------------------------

export interface CandidateSource {
  source_chunk_id: string;
  source_order: number;
  evidence_quote?: string;
}

export interface ContentCandidate {
  candidate_id: string;
  document_id?: string;
  version_id?: string;
  candidate_type: CandidateType;
  status: CandidateStatus;
  source_content_origin: ContentOrigin;
  trust_level: TrustLevel;
  title: string;
  summary?: string;
  reason?: string;
  target_knowledge_point_id?: string;
  merged_into_candidate_id?: string;
  confirmed_outcome?: CandidateOutcome;
  decision_note?: string;
  extractor_model?: string;
  extractor_version?: string;
  sources: CandidateSource[];
  confirmed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface KnowledgePoint {
  knowledge_point_id: string;
  title: string;
  description?: string;
  status: string;
  // mastery_ui_state 恒为 no_evidence（暂无证据），不是掌握等级。
  mastery_ui_state: string;
  created_at: string;
  updated_at: string;
}

export interface ExtractCandidatesResult {
  candidates: ContentCandidate[];
  allowed_candidate_types: CandidateType[];
  proposed: number;
  filtered: number;
  duplicated: number;
}

export interface ConfirmCandidateResult {
  candidate: ContentCandidate;
  knowledge_point?: KnowledgePoint;
}

export function extractCandidates(
  documentID: string,
): Promise<ExtractCandidatesResult> {
  return sendJSON<ExtractCandidatesResult>(
    "POST",
    `/documents/${documentID}/candidates/extract`,
    undefined,
    "抽取候选内容失败",
  );
}

export function listCandidates(filter?: {
  document_id?: string;
  status?: CandidateStatus;
  candidate_type?: CandidateType;
}): Promise<ContentCandidate[]> {
  const params = new URLSearchParams();
  if (filter?.document_id) params.set("document_id", filter.document_id);
  if (filter?.status) params.set("status", filter.status);
  if (filter?.candidate_type) params.set("candidate_type", filter.candidate_type);
  const query = params.toString();
  return getJSON<ContentCandidate[]>(
    `/candidates${query ? `?${query}` : ""}`,
    "查询候选列表失败",
  );
}

export function getCandidate(candidateID: string): Promise<ContentCandidate> {
  return getJSON<ContentCandidate>(
    `/candidates/${candidateID}`,
    "查询候选内容失败",
  );
}

export function updateCandidate(
  candidateID: string,
  payload: { title: string; summary?: string; reason?: string },
): Promise<ContentCandidate> {
  return sendJSON<ContentCandidate>(
    "PATCH",
    `/candidates/${candidateID}`,
    payload,
    "修改候选内容失败",
  );
}

// confirmCandidate 是候选进入正式链路的唯一入口。
// 传 knowledge_point_id 表示关联已有知识点；不传则按候选类型走各自边界。
export function confirmCandidate(
  candidateID: string,
  payload?: {
    knowledge_point_id?: string;
    payload?: { title: string; summary?: string; reason?: string };
    decision_note?: string;
  },
): Promise<ConfirmCandidateResult> {
  return sendJSON<ConfirmCandidateResult>(
    "POST",
    `/candidates/${candidateID}/confirm`,
    payload ?? {},
    "确认候选内容失败",
  );
}

export function mergeCandidate(
  candidateID: string,
  intoCandidateID: string,
  decisionNote?: string,
): Promise<ContentCandidate> {
  return sendJSON<ContentCandidate>(
    "POST",
    `/candidates/${candidateID}/merge`,
    { into_candidate_id: intoCandidateID, decision_note: decisionNote ?? "" },
    "合并候选内容失败",
  );
}

export function archiveCandidate(
  candidateID: string,
  decisionNote?: string,
): Promise<ContentCandidate> {
  return sendJSON<ContentCandidate>(
    "POST",
    `/candidates/${candidateID}/archive`,
    { decision_note: decisionNote ?? "" },
    "归档候选内容失败",
  );
}

export function rejectCandidate(
  candidateID: string,
  decisionNote?: string,
): Promise<ContentCandidate> {
  return sendJSON<ContentCandidate>(
    "POST",
    `/candidates/${candidateID}/reject`,
    { decision_note: decisionNote ?? "" },
    "拒绝候选内容失败",
  );
}

export function listKnowledgePoints(): Promise<KnowledgePoint[]> {
  return getJSON<KnowledgePoint[]>("/knowledge-points", "查询知识点列表失败");
}

// ---------------------------------------------------------------------------
// 前端侧规则辅助
// ---------------------------------------------------------------------------

// normalizePurposes 复刻后端的互斥规则：勾了「仅归档」就只能是「仅归档」。
// 前端先拦一道只为交互即时反馈，真正的裁决仍然在后端。
export function normalizePurposes(
  purposes: DocumentPurpose[],
): DocumentPurpose[] {
  if (purposes.includes("archive_only")) return ["archive_only"];
  return DOCUMENT_PURPOSE_OPTIONS.filter((p) => purposes.includes(p));
}

// newIdempotencyKey 生成上传幂等键。同一次用户操作的重试必须复用同一个键。
export function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `upload-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}
