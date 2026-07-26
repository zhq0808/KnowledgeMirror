-- ============================================================================
-- 000009 · Agent 知识库检索 v0 审计
-- 本迁移只新增「检索发生过什么」的审计表，不改变任何授权、资料或掌握状态语义。
-- 可召回集合完全由已有事实推导（documents.deleted_at / current_version_id
-- + document_usages.ai_retrieval + source_chunks.retrieval_enabled），
-- 因此不引入任何检索索引副本，避免出现「授权已撤回但副本仍可召回」的残片。
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 9.1 检索请求：一次检索一行，用于回答对账与是否引入 pgvector 的决策依据
-- ---------------------------------------------------------------------------
CREATE TABLE retrieval_requests (
    retrieval_request_id UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id              VARCHAR(64)  NOT NULL,             -- 数据归属用户
    session_id           VARCHAR(64),                       -- 检索预览等场景可为空
    trace_id             VARCHAR(64),
    purpose              VARCHAR(30)  NOT NULL DEFAULT 'ai_retrieval',
    knowledge_point_id   UUID,
    query_text           TEXT         NOT NULL,             -- 已按上限截断；不落片段正文
    query_terms          TEXT[]       NOT NULL DEFAULT ARRAY[]::TEXT[],
    max_results          INTEGER      NOT NULL,
    context_budget_chars INTEGER      NOT NULL,
    candidate_count      INTEGER      NOT NULL DEFAULT 0,   -- 数据库返回的候选数，=0 表示零命中
    selected_count       INTEGER      NOT NULL DEFAULT 0,   -- 实际进入 Prompt 的片段数
    excluded_count       INTEGER      NOT NULL DEFAULT 0,
    prompt_chars         INTEGER      NOT NULL DEFAULT 0,   -- 上下文成本
    duration_ms          INTEGER      NOT NULL DEFAULT 0,
    status               VARCHAR(20)  NOT NULL DEFAULT 'ok',
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT ck_retrieval_requests_purpose CHECK (purpose IN (
        'ai_retrieval', 'generate_plan'
    )),
    CONSTRAINT ck_retrieval_requests_status CHECK (status IN ('ok', 'empty', 'failed')),
    CONSTRAINT ck_retrieval_requests_counts CHECK (
        max_results > 0
        AND context_budget_chars > 0
        AND candidate_count >= 0
        AND selected_count >= 0
        AND excluded_count >= 0
        AND prompt_chars >= 0
        AND duration_ms >= 0
    ),
    CONSTRAINT fk_retrieval_requests_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_retrieval_requests_session FOREIGN KEY (session_id, user_id)
        REFERENCES agent_memory_session(session_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_retrieval_requests_knowledge_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_retrieval_requests_user_created
    ON retrieval_requests (user_id, created_at DESC);
-- 召回质量看板：按状态统计零命中占比与延迟分布；真正漏召需由固定评测集判定。
CREATE INDEX idx_retrieval_requests_status_created
    ON retrieval_requests (status, created_at DESC);

COMMENT ON TABLE retrieval_requests IS '知识库检索请求审计；检索命中不产生任何掌握状态';
COMMENT ON COLUMN retrieval_requests.query_text IS '用户查询原文（按上限截断）；片段正文不复制到审计表';
COMMENT ON COLUMN retrieval_requests.candidate_count IS '数据库返回的候选片段数；为 0 表示零命中，不等同于已确认漏召';
COMMENT ON COLUMN retrieval_requests.prompt_chars IS '实际进入 Prompt 的字符数，用于评估上下文成本';

-- ---------------------------------------------------------------------------
-- 9.2 检索命中明细：哪些片段被选中、哪些被隔离或被预算裁掉
-- ---------------------------------------------------------------------------
CREATE TABLE retrieval_hits (
    retrieval_request_id UUID         NOT NULL,
    source_chunk_id      UUID         NOT NULL,
    user_id              VARCHAR(64)  NOT NULL,
    document_id          UUID         NOT NULL,
    document_version_id  UUID         NOT NULL,
    ref                  VARCHAR(20),                       -- 进入 Prompt 时的稳定编号，如 S1
    rank                 INTEGER      NOT NULL,             -- 1 起，按排序分数降序
    score                NUMERIC(10,4) NOT NULL DEFAULT 0,
    included_in_prompt   BOOLEAN      NOT NULL DEFAULT FALSE,
    excluded_reason      VARCHAR(40),
    char_cost            INTEGER      NOT NULL DEFAULT 0,
    truncated            BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT pk_retrieval_hits PRIMARY KEY (retrieval_request_id, source_chunk_id),
    CONSTRAINT ck_retrieval_hits_rank CHECK (rank > 0),
    CONSTRAINT ck_retrieval_hits_char_cost CHECK (char_cost >= 0),
    -- 进入 Prompt 与被排除互斥：一条命中不可能同时“用了”和“因故没用”。
    CONSTRAINT ck_retrieval_hits_exclusion CHECK (
        (included_in_prompt AND excluded_reason IS NULL AND ref IS NOT NULL)
        OR (NOT included_in_prompt AND excluded_reason IS NOT NULL AND ref IS NULL)
    ),
    CONSTRAINT ck_retrieval_hits_reason CHECK (excluded_reason IS NULL OR excluded_reason IN (
        'prompt_injection', 'document_quota', 'context_budget', 'result_quota', 'empty_content'
    )),
    CONSTRAINT fk_retrieval_hits_request FOREIGN KEY (retrieval_request_id)
        REFERENCES retrieval_requests(retrieval_request_id) ON DELETE CASCADE,
    -- 片段血缘永久保留，所以审计明细可以安全外键引用它。
    CONSTRAINT fk_retrieval_hits_chunk FOREIGN KEY (source_chunk_id, user_id)
        REFERENCES source_chunks(source_chunk_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_retrieval_hits_chunk
    ON retrieval_hits (source_chunk_id, created_at DESC);
-- 注入隔离与预算裁剪的趋势监控。
CREATE INDEX idx_retrieval_hits_excluded
    ON retrieval_hits (excluded_reason, created_at DESC)
    WHERE excluded_reason IS NOT NULL;

COMMENT ON TABLE retrieval_hits IS '单次检索的候选片段明细；included_in_prompt=false 时必须写明排除原因';
COMMENT ON COLUMN retrieval_hits.excluded_reason IS 'prompt_injection 表示命中疑似注入指令被隔离，不是普通预算裁剪';
