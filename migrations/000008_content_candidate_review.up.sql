-- ============================================================================
-- 000008 · 候选内容确认
-- AI 只能提出候选；知识点、计划、可信事实、生产证据和状态变化都必须由用户确认。
-- 本迁移把这条规则从「服务层约定」下沉为「数据库不变式」：
--   1. 候选必须引用至少一个来源片段（延迟约束触发器，在事务提交时校验）；
--   2. 候选必须能回到具体资料版本；
--   3. AI 整理来源和项目事实候选的可信级别永远停在 unverified；
--   4. 候选一旦被处理（确认/关联/合并/归档/拒绝）就不可再改，血缘字段永久不可变。
-- 本迁移不创建任何掌握状态或掌握证据表。
-- ============================================================================

ALTER TABLE content_candidates
    ADD COLUMN document_id              UUID,
    ADD COLUMN document_version_id      UUID,
    ADD COLUMN source_content_origin    VARCHAR(30) NOT NULL DEFAULT 'pending_confirmation',
    ADD COLUMN trust_level              VARCHAR(30) NOT NULL DEFAULT 'unverified',
    ADD COLUMN confirmed_outcome        VARCHAR(40),
    ADD COLUMN merged_into_candidate_id UUID,
    ADD COLUMN decision_note            TEXT,
    ADD COLUMN dedupe_hash              BYTEA;

ALTER TABLE content_candidates
    -- 资料血缘：候选来自哪个资料的哪个版本，要么两个都有，要么都没有。
    ADD CONSTRAINT ck_content_candidates_document CHECK (
        (document_id IS NULL AND document_version_id IS NULL)
        OR (document_id IS NOT NULL AND document_version_id IS NOT NULL)
    ),
    ADD CONSTRAINT ck_content_candidates_origin CHECK (source_content_origin IN (
        'user_authored', 'ai_generated', 'external', 'pending_confirmation'
    )),
    -- 候选层最高只能到 user_confirmed；trusted 属于知识点与证据层，候选确认不得越级。
    ADD CONSTRAINT ck_content_candidates_trust CHECK (trust_level IN (
        'unverified', 'user_confirmed'
    )),
    -- AI 整理的内容即使被确认，也不自动提升可信级别。
    ADD CONSTRAINT ck_content_candidates_ai_trust CHECK (
        source_content_origin <> 'ai_generated' OR trust_level = 'unverified'
    ),
    -- 项目事实只能是「待核实事实」，不能因为用户点了确认就变成可信事实或生产证据。
    ADD CONSTRAINT ck_content_candidates_fact_trust CHECK (
        candidate_type <> 'personal_fact' OR trust_level = 'unverified'
    ),
    ADD CONSTRAINT ck_content_candidates_outcome CHECK (
        (status = 'pending' AND confirmed_outcome IS NULL)
        OR (status <> 'pending' AND confirmed_outcome IS NOT NULL)
    ),
    ADD CONSTRAINT ck_content_candidates_outcome_value CHECK (
        confirmed_outcome IS NULL OR confirmed_outcome IN (
            'knowledge_point_created', 'knowledge_point_linked',
            'plan_task_pending_intake', 'jd_requirement_pending_intake',
            'unverified_fact', 'reference_only',
            'merged', 'archived', 'rejected'
        )
    ),
    ADD CONSTRAINT ck_content_candidates_merge CHECK (
        (status = 'merged') = (merged_into_candidate_id IS NOT NULL)
    ),
    ADD CONSTRAINT ck_content_candidates_merge_self CHECK (
        merged_into_candidate_id IS NULL OR merged_into_candidate_id <> candidate_id
    ),
    ADD CONSTRAINT ck_content_candidates_linked_target CHECK (
        status <> 'linked' OR target_knowledge_point_id IS NOT NULL
    ),
    ADD CONSTRAINT ck_content_candidates_note CHECK (
        decision_note IS NULL OR btrim(decision_note) <> ''
    ),
    ADD CONSTRAINT ck_content_candidates_dedupe CHECK (
        dedupe_hash IS NULL OR octet_length(dedupe_hash) = 32
    ),
    ADD CONSTRAINT fk_content_candidates_version FOREIGN KEY (document_version_id, document_id, user_id)
        REFERENCES document_versions(version_id, document_id, user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_content_candidates_merged_into FOREIGN KEY (merged_into_candidate_id, user_id)
        REFERENCES content_candidates(candidate_id, user_id) ON DELETE RESTRICT;

COMMENT ON COLUMN content_candidates.source_content_origin IS '抽取时资料来源的快照；ai_generated 会永久锁死 trust_level';
COMMENT ON COLUMN content_candidates.trust_level IS '候选可信级别，最高 user_confirmed；trusted/production 不属于候选层';
COMMENT ON COLUMN content_candidates.confirmed_outcome IS '用户处理后实际产生了什么；plan_task/jd_requirement 只到「待接入」，不写计划';
COMMENT ON COLUMN content_candidates.dedupe_hash IS '类型+标题+来源片段的哈希；重复抽取不会产生重复的待确认项';

-- 重复抽取同一版本不产生重复待确认项；被处理掉的候选不占用该唯一键。
CREATE UNIQUE INDEX uk_content_candidates_pending_dedupe
    ON content_candidates (user_id, document_version_id, dedupe_hash)
    WHERE status = 'pending' AND dedupe_hash IS NOT NULL;

CREATE INDEX idx_content_candidates_document
    ON content_candidates (user_id, document_id, status, created_at);

-- ---------------------------------------------------------------------------
-- 候选一旦被处理就不可再改；血缘字段任何时候都不可改。
-- ---------------------------------------------------------------------------
CREATE FUNCTION enforce_content_candidate_transition() RETURNS trigger AS $$
BEGIN
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'content candidate % is already resolved as %', OLD.candidate_id, OLD.status
            USING ERRCODE = '55000';
    END IF;

    IF NEW.candidate_id IS DISTINCT FROM OLD.candidate_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id
        OR NEW.candidate_type IS DISTINCT FROM OLD.candidate_type
        OR NEW.document_id IS DISTINCT FROM OLD.document_id
        OR NEW.document_version_id IS DISTINCT FROM OLD.document_version_id
        OR NEW.source_content_origin IS DISTINCT FROM OLD.source_content_origin THEN
        RAISE EXCEPTION 'content candidate lineage fields are immutable'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_content_candidates_transition
    BEFORE UPDATE ON content_candidates
    FOR EACH ROW EXECUTE FUNCTION enforce_content_candidate_transition();

-- ---------------------------------------------------------------------------
-- 「每个候选必须引用一个或多个来源片段」。
-- 用延迟约束触发器：候选行和引用行可以在同一事务内按任意顺序写入，
-- 但事务提交时缺引用的候选一定失败，杜绝“无出处的候选”。
-- ---------------------------------------------------------------------------
CREATE FUNCTION require_content_candidate_source() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM content_candidate_sources
        WHERE candidate_id = NEW.candidate_id
    ) THEN
        RAISE EXCEPTION 'content candidate % must reference at least one source chunk', NEW.candidate_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trg_content_candidates_require_source
    AFTER INSERT ON content_candidates
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION require_content_candidate_source();

-- 候选的来源引用是血缘数据，不允许删除或改指向。
CREATE FUNCTION protect_content_candidate_source() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'content candidate sources are retained for lineage'
            USING ERRCODE = '55000';
    END IF;
    IF NEW.candidate_id IS DISTINCT FROM OLD.candidate_id
        OR NEW.source_chunk_id IS DISTINCT FROM OLD.source_chunk_id
        OR NEW.user_id IS DISTINCT FROM OLD.user_id THEN
        RAISE EXCEPTION 'content candidate source lineage is immutable'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_content_candidate_sources_lineage
    BEFORE UPDATE OR DELETE ON content_candidate_sources
    FOR EACH ROW EXECUTE FUNCTION protect_content_candidate_source();
