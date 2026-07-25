-- 回滚 000008：移除候选确认相关的约束、索引、触发器与列。

DROP TRIGGER IF EXISTS trg_content_candidate_sources_lineage ON content_candidate_sources;
DROP FUNCTION IF EXISTS protect_content_candidate_source();

DROP TRIGGER IF EXISTS trg_content_candidates_require_source ON content_candidates;
DROP FUNCTION IF EXISTS require_content_candidate_source();

DROP TRIGGER IF EXISTS trg_content_candidates_transition ON content_candidates;
DROP FUNCTION IF EXISTS enforce_content_candidate_transition();

DROP INDEX IF EXISTS idx_content_candidates_document;
DROP INDEX IF EXISTS uk_content_candidates_pending_dedupe;

ALTER TABLE content_candidates
    DROP CONSTRAINT IF EXISTS fk_content_candidates_merged_into,
    DROP CONSTRAINT IF EXISTS fk_content_candidates_version,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_dedupe,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_note,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_linked_target,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_merge_self,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_merge,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_outcome_value,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_outcome,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_fact_trust,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_ai_trust,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_trust,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_origin,
    DROP CONSTRAINT IF EXISTS ck_content_candidates_document;

ALTER TABLE content_candidates
    DROP COLUMN IF EXISTS dedupe_hash,
    DROP COLUMN IF EXISTS decision_note,
    DROP COLUMN IF EXISTS merged_into_candidate_id,
    DROP COLUMN IF EXISTS confirmed_outcome,
    DROP COLUMN IF EXISTS trust_level,
    DROP COLUMN IF EXISTS source_content_origin,
    DROP COLUMN IF EXISTS document_version_id,
    DROP COLUMN IF EXISTS document_id;
