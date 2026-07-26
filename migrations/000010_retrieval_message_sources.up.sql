-- 让已落库的检索审计可以原样恢复回答使用的 S1/S2 编号与截断状态。
ALTER TABLE retrieval_hits
    ADD COLUMN IF NOT EXISTS ref VARCHAR(20),
    ADD COLUMN IF NOT EXISTS truncated BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE retrieval_hits
    DROP CONSTRAINT IF EXISTS ck_retrieval_hits_exclusion;

ALTER TABLE retrieval_hits
    ADD CONSTRAINT ck_retrieval_hits_exclusion CHECK (
        (included_in_prompt AND excluded_reason IS NULL AND ref IS NOT NULL)
        OR (NOT included_in_prompt AND excluded_reason IS NOT NULL AND ref IS NULL)
    );

COMMENT ON COLUMN retrieval_hits.ref IS '进入 Prompt 时的稳定片段编号，如 S1；被排除候选为空';
COMMENT ON COLUMN retrieval_hits.truncated IS '进入 Prompt 的片段正文是否被显式截断';