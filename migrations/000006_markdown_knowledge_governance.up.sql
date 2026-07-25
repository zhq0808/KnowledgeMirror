-- ============================================================================
-- 000006 · Markdown 资料治理与持久化
-- 原文 -> 版本 -> 来源片段 -> 用途确认 -> 候选内容 -> 正式知识点。
-- 资料导入、解析和候选确认均不在本迁移中创建掌握状态或掌握证据。
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 6.1 资料主记录
-- ---------------------------------------------------------------------------
CREATE TABLE documents (
    document_id       UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id           VARCHAR(64)  NOT NULL,             -- 数据归属用户
    title             TEXT         NOT NULL,
    content_origin    VARCHAR(30)  NOT NULL DEFAULT 'pending_confirmation',
    document_kind     VARCHAR(30)  NOT NULL DEFAULT 'other',
    status            VARCHAR(30)  NOT NULL DEFAULT 'uploaded',
    current_version_id UUID,                              -- 创建首个版本后在同一事务内回填

    created_by        VARCHAR(64)  NOT NULL,
    updated_by        VARCHAR(64)  NOT NULL,
    deleted_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_documents_owner UNIQUE (document_id, user_id),
    CONSTRAINT ck_documents_title CHECK (btrim(title) <> ''),
    CONSTRAINT ck_documents_origin CHECK (content_origin IN (
        'user_authored', 'ai_generated', 'external', 'pending_confirmation'
    )),
    CONSTRAINT ck_documents_kind CHECK (document_kind IN (
        'learning_note', 'learning_todo', 'technical_material', 'target_jd',
        'project_fact', 'interview_review', 'other'
    )),
    CONSTRAINT ck_documents_status CHECK (status IN (
        'uploaded', 'parsing', 'pending_confirmation', 'ready', 'failed', 'archived'
    )),
    CONSTRAINT ck_documents_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_documents_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_documents_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_documents_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_documents_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_documents_user_updated
    ON documents (user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TRIGGER trg_documents_updated_at
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE documents IS '用户资料主记录；资料存在不代表已形成知识点或掌握证据';
COMMENT ON COLUMN documents.content_origin IS '内容来源: user_authored/ai_generated/external/pending_confirmation';
COMMENT ON COLUMN documents.document_kind IS '内容类别，不承担用途或掌握状态语义';
COMMENT ON COLUMN documents.current_version_id IS '当前展示版本；历史版本仍永久保留';

-- ---------------------------------------------------------------------------
-- 6.2 追加式资料版本；Markdown v0 原文以 PostgreSQL TEXT 为事实源
-- ---------------------------------------------------------------------------
CREATE TABLE document_versions (
    version_id         UUID         PRIMARY KEY,          -- 后端 UUIDv7
    document_id        UUID         NOT NULL,
    user_id            VARCHAR(64)  NOT NULL,
    version_no         INTEGER      NOT NULL,
    original_filename  TEXT         NOT NULL,
    mime_type          VARCHAR(100) NOT NULL,
    size_bytes         BIGINT       NOT NULL,
    sha256             BYTEA        NOT NULL,
    raw_text           TEXT         NOT NULL,
    parser_version     VARCHAR(50)  NOT NULL,

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_document_versions_number UNIQUE (document_id, version_no),
    CONSTRAINT uk_document_versions_owner UNIQUE (version_id, document_id, user_id),
    CONSTRAINT ck_document_versions_number CHECK (version_no > 0),
    CONSTRAINT ck_document_versions_filename CHECK (btrim(original_filename) <> ''),
    CONSTRAINT ck_document_versions_mime CHECK (btrim(mime_type) <> ''),
    CONSTRAINT ck_document_versions_size CHECK (size_bytes >= 0),
    CONSTRAINT ck_document_versions_sha256 CHECK (octet_length(sha256) = 32),
    CONSTRAINT ck_document_versions_parser CHECK (btrim(parser_version) <> ''),
    CONSTRAINT ck_document_versions_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_document_versions_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_document_versions_document FOREIGN KEY (document_id, user_id)
        REFERENCES documents(document_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_versions_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_versions_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_document_versions_document
    ON document_versions (document_id, version_no DESC);
-- 非唯一索引：用于发现同一用户的重复内容，但不阻止用户保存两份独立资料。
CREATE INDEX idx_document_versions_user_sha256
    ON document_versions (user_id, sha256);

ALTER TABLE documents
    ADD CONSTRAINT fk_documents_current_version
    FOREIGN KEY (current_version_id, document_id, user_id)
    REFERENCES document_versions(version_id, document_id, user_id)
    ON DELETE RESTRICT
    DEFERRABLE INITIALLY DEFERRED;

COMMENT ON TABLE document_versions IS '资料追加式版本；Markdown v0 原文保存在 raw_text，不写 Redis';
COMMENT ON COLUMN document_versions.sha256 IS '原始 UTF-8 文件字节的 SHA-256（32 字节），用于查重提示而非静默合并';
COMMENT ON COLUMN document_versions.raw_text IS 'Markdown UTF-8 原文，PostgreSQL TEXT 是 v0 事实源';

CREATE FUNCTION reject_document_version_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'document_versions are append-only'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_document_versions_append_only
    BEFORE UPDATE OR DELETE ON document_versions
    FOR EACH ROW EXECUTE FUNCTION reject_document_version_mutation();

-- ---------------------------------------------------------------------------
-- 6.3 资料版本用途；一行表达一个用途，允许多选
-- ---------------------------------------------------------------------------
CREATE TABLE document_usages (
    usage_id           UUID         PRIMARY KEY,          -- 后端 UUIDv7
    document_version_id UUID        NOT NULL,
    document_id        UUID         NOT NULL,
    user_id            VARCHAR(64)  NOT NULL,
    purpose            VARCHAR(30)  NOT NULL,
    enabled            BOOLEAN      NOT NULL DEFAULT FALSE,
    confirmed_by       VARCHAR(64),
    confirmed_at       TIMESTAMPTZ,

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_document_usages_purpose UNIQUE (document_version_id, purpose),
    CONSTRAINT ck_document_usages_purpose CHECK (purpose IN (
        'learn', 'ai_retrieval', 'generate_plan', 'fact_reference', 'archive_only'
    )),
    CONSTRAINT ck_document_usages_confirmation CHECK (
        (confirmed_by IS NULL AND confirmed_at IS NULL)
        OR (confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
    ),
    CONSTRAINT ck_document_usages_confirmed_owner CHECK (confirmed_by IS NULL OR confirmed_by = user_id),
    CONSTRAINT ck_document_usages_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_document_usages_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_document_usages_version FOREIGN KEY (document_version_id, document_id, user_id)
        REFERENCES document_versions(version_id, document_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_usages_confirmed_by FOREIGN KEY (confirmed_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_usages_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_usages_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_document_usages_enabled
    ON document_usages (user_id, purpose, document_version_id)
    WHERE enabled;

CREATE TRIGGER trg_document_usages_updated_at
    BEFORE UPDATE ON document_usages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE FUNCTION enforce_archive_only_usage() RETURNS trigger AS $$
BEGIN
    IF NOT NEW.enabled THEN
        RETURN NEW;
    END IF;

    -- 同一版本的用途修改串行化，避免并发事务同时绕过互斥检查。
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.document_version_id::text, 0));

    IF NEW.purpose = 'archive_only' AND EXISTS (
        SELECT 1 FROM document_usages
        WHERE document_version_id = NEW.document_version_id
          AND usage_id <> NEW.usage_id
          AND enabled
          AND purpose <> 'archive_only'
    ) THEN
        RAISE EXCEPTION 'archive_only cannot be enabled with other purposes'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.purpose <> 'archive_only' AND EXISTS (
        SELECT 1 FROM document_usages
        WHERE document_version_id = NEW.document_version_id
          AND usage_id <> NEW.usage_id
          AND enabled
          AND purpose = 'archive_only'
    ) THEN
        RAISE EXCEPTION 'other purposes cannot be enabled with archive_only'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_document_usages_archive_only
    BEFORE INSERT OR UPDATE OF purpose, enabled ON document_usages
    FOR EACH ROW EXECUTE FUNCTION enforce_archive_only_usage();

COMMENT ON TABLE document_usages IS '资料版本的多选用途及用户确认记录；用途不产生掌握状态';

-- ---------------------------------------------------------------------------
-- 6.4 可追溯来源片段
-- ---------------------------------------------------------------------------
CREATE TABLE source_chunks (
    source_chunk_id    UUID         PRIMARY KEY,          -- 后端 UUIDv7
    document_version_id UUID        NOT NULL,
    document_id        UUID         NOT NULL,
    user_id            VARCHAR(64)  NOT NULL,
    ordinal            INTEGER      NOT NULL,
    heading_path       TEXT[]       NOT NULL DEFAULT ARRAY[]::TEXT[],
    content            TEXT         NOT NULL,
    start_offset       BIGINT       NOT NULL,             -- raw_text 字符偏移，左闭
    end_offset         BIGINT       NOT NULL,             -- raw_text 字符偏移，右开
    content_hash       BYTEA        NOT NULL,
    trust_level        VARCHAR(30)  NOT NULL DEFAULT 'unverified',
    retrieval_enabled  BOOLEAN      NOT NULL DEFAULT FALSE,

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_source_chunks_ordinal UNIQUE (document_version_id, ordinal),
    CONSTRAINT uk_source_chunks_owner UNIQUE (source_chunk_id, user_id),
    CONSTRAINT ck_source_chunks_ordinal CHECK (ordinal > 0),
    CONSTRAINT ck_source_chunks_content CHECK (btrim(content) <> ''),
    CONSTRAINT ck_source_chunks_offsets CHECK (start_offset >= 0 AND end_offset > start_offset),
    CONSTRAINT ck_source_chunks_hash CHECK (octet_length(content_hash) = 32),
    CONSTRAINT ck_source_chunks_trust CHECK (trust_level IN (
        'unverified', 'user_confirmed', 'trusted'
    )),
    CONSTRAINT ck_source_chunks_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_source_chunks_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_source_chunks_version FOREIGN KEY (document_version_id, document_id, user_id)
        REFERENCES document_versions(version_id, document_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_source_chunks_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_source_chunks_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_source_chunks_version
    ON source_chunks (document_version_id, ordinal);
CREATE INDEX idx_source_chunks_retrieval
    ON source_chunks (user_id, document_version_id, ordinal)
    WHERE retrieval_enabled;

CREATE TRIGGER trg_source_chunks_updated_at
    BEFORE UPDATE ON source_chunks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE FUNCTION protect_source_chunk_lineage() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'source_chunks are retained for lineage'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.document_version_id IS DISTINCT FROM OLD.document_version_id
        OR NEW.document_id IS DISTINCT FROM OLD.document_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id
        OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
        OR NEW.heading_path IS DISTINCT FROM OLD.heading_path
        OR NEW.content IS DISTINCT FROM OLD.content
        OR NEW.start_offset IS DISTINCT FROM OLD.start_offset
        OR NEW.end_offset IS DISTINCT FROM OLD.end_offset
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash THEN
        RAISE EXCEPTION 'source chunk lineage fields are immutable'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_source_chunks_lineage
    BEFORE UPDATE OR DELETE ON source_chunks
    FOR EACH ROW EXECUTE FUNCTION protect_source_chunk_lineage();

COMMENT ON TABLE source_chunks IS '从特定资料版本解析出的可追溯原文片段；检索开关默认关闭';
COMMENT ON COLUMN source_chunks.heading_path IS 'Markdown AST 标题层级路径';
COMMENT ON COLUMN source_chunks.start_offset IS '片段在该版本 raw_text 中的字符起始偏移（左闭）';
COMMENT ON COLUMN source_chunks.end_offset IS '片段在该版本 raw_text 中的字符结束偏移（右开）';
COMMENT ON COLUMN source_chunks.trust_level IS '可信级别；AI 生成或解析不会自动提升该字段';

-- ---------------------------------------------------------------------------
-- 6.5 正式知识点及来源关系；不在此表存“暂无证据”或掌握等级
-- ---------------------------------------------------------------------------
CREATE TABLE knowledge_points (
    knowledge_point_id UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id            VARCHAR(64)  NOT NULL,
    title              TEXT         NOT NULL,
    description        TEXT,
    status             VARCHAR(20)  NOT NULL DEFAULT 'active',

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    deleted_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_knowledge_points_owner UNIQUE (knowledge_point_id, user_id),
    CONSTRAINT ck_knowledge_points_title CHECK (btrim(title) <> ''),
    CONSTRAINT ck_knowledge_points_status CHECK (status IN ('active', 'archived')),
    CONSTRAINT ck_knowledge_points_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_knowledge_points_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_knowledge_points_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_points_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_points_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_knowledge_points_user_updated
    ON knowledge_points (user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TRIGGER trg_knowledge_points_updated_at
    BEFORE UPDATE ON knowledge_points
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE knowledge_point_sources (
    knowledge_point_id UUID         NOT NULL,
    source_chunk_id    UUID         NOT NULL,
    user_id            VARCHAR(64)  NOT NULL,
    relation_type      VARCHAR(20)  NOT NULL DEFAULT 'reference',

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT pk_knowledge_point_sources PRIMARY KEY (knowledge_point_id, source_chunk_id),
    CONSTRAINT ck_knowledge_point_sources_type CHECK (relation_type IN ('primary', 'reference')),
    CONSTRAINT ck_knowledge_point_sources_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_knowledge_point_sources_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_knowledge_point_sources_point FOREIGN KEY (knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_point_sources_chunk FOREIGN KEY (source_chunk_id, user_id)
        REFERENCES source_chunks(source_chunk_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_point_sources_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_knowledge_point_sources_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_knowledge_point_sources_chunk
    ON knowledge_point_sources (source_chunk_id, knowledge_point_id);

CREATE TRIGGER trg_knowledge_point_sources_updated_at
    BEFORE UPDATE ON knowledge_point_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE knowledge_points IS '用户确认后正式追踪的知识点；创建后初始 UI 为“暂无证据”';
COMMENT ON TABLE knowledge_point_sources IS '正式知识点与一个或多个来源片段的可追溯关联';

-- ---------------------------------------------------------------------------
-- 6.6 AI 提出的候选内容及多来源引用
-- ---------------------------------------------------------------------------
CREATE TABLE content_candidates (
    candidate_id        UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id             VARCHAR(64)  NOT NULL,
    candidate_type      VARCHAR(30)  NOT NULL,
    candidate_payload   JSONB        NOT NULL,
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    target_knowledge_point_id UUID,
    extractor_model     VARCHAR(100),
    extractor_version   VARCHAR(50),
    confirmed_by        VARCHAR(64),
    confirmed_at        TIMESTAMPTZ,

    created_by          VARCHAR(64)  NOT NULL,
    updated_by          VARCHAR(64)  NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_content_candidates_owner UNIQUE (candidate_id, user_id),
    CONSTRAINT ck_content_candidates_type CHECK (candidate_type IN (
        'knowledge_point', 'plan_task', 'jd_requirement', 'personal_fact', 'reference_only'
    )),
    CONSTRAINT ck_content_candidates_payload CHECK (jsonb_typeof(candidate_payload) = 'object'),
    CONSTRAINT ck_content_candidates_status CHECK (status IN (
        'pending', 'confirmed', 'linked', 'merged', 'archived', 'rejected'
    )),
    CONSTRAINT ck_content_candidates_confirmation CHECK (
        (status = 'pending' AND confirmed_by IS NULL AND confirmed_at IS NULL)
        OR (status <> 'pending' AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
    ),
    CONSTRAINT ck_content_candidates_confirmed_owner CHECK (confirmed_by IS NULL OR confirmed_by = user_id),
    CONSTRAINT ck_content_candidates_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_content_candidates_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_content_candidates_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidates_target FOREIGN KEY (target_knowledge_point_id, user_id)
        REFERENCES knowledge_points(knowledge_point_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidates_confirmed_by FOREIGN KEY (confirmed_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidates_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidates_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_content_candidates_pending
    ON content_candidates (user_id, created_at)
    WHERE status = 'pending';

CREATE TRIGGER trg_content_candidates_updated_at
    BEFORE UPDATE ON content_candidates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE content_candidate_sources (
    candidate_id       UUID         NOT NULL,
    source_chunk_id    UUID         NOT NULL,
    user_id            VARCHAR(64)  NOT NULL,
    source_order       SMALLINT     NOT NULL,
    evidence_quote     TEXT,

    created_by         VARCHAR(64)  NOT NULL,
    updated_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT pk_content_candidate_sources PRIMARY KEY (candidate_id, source_chunk_id),
    CONSTRAINT uk_content_candidate_sources_order UNIQUE (candidate_id, source_order),
    CONSTRAINT ck_content_candidate_sources_order CHECK (source_order > 0),
    CONSTRAINT ck_content_candidate_sources_quote CHECK (
        evidence_quote IS NULL OR (btrim(evidence_quote) <> '' AND char_length(evidence_quote) <= 2000)
    ),
    CONSTRAINT ck_content_candidate_sources_created_owner CHECK (created_by = user_id),
    CONSTRAINT ck_content_candidate_sources_updated_owner CHECK (updated_by = user_id),
    CONSTRAINT fk_content_candidate_sources_candidate FOREIGN KEY (candidate_id, user_id)
        REFERENCES content_candidates(candidate_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidate_sources_chunk FOREIGN KEY (source_chunk_id, user_id)
        REFERENCES source_chunks(source_chunk_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidate_sources_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_content_candidate_sources_updated_by FOREIGN KEY (updated_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_content_candidate_sources_chunk
    ON content_candidate_sources (source_chunk_id, candidate_id);

CREATE TRIGGER trg_content_candidate_sources_updated_at
    BEFORE UPDATE ON content_candidate_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE content_candidates IS 'AI 提出的待确认内容；不会直接写入知识点、计划、可信事实或掌握状态';
COMMENT ON COLUMN content_candidates.candidate_type IS 'knowledge_point/plan_task/jd_requirement/personal_fact/reference_only';
COMMENT ON TABLE content_candidate_sources IS '候选内容引用的一个或多个来源片段';
