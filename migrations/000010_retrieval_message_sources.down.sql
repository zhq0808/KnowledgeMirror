ALTER TABLE retrieval_hits
    DROP CONSTRAINT IF EXISTS ck_retrieval_hits_exclusion;

ALTER TABLE retrieval_hits
    ADD CONSTRAINT ck_retrieval_hits_exclusion CHECK (
        (included_in_prompt AND excluded_reason IS NULL)
        OR (NOT included_in_prompt AND excluded_reason IS NOT NULL)
    );

ALTER TABLE retrieval_hits
    DROP COLUMN IF EXISTS truncated,
    DROP COLUMN IF EXISTS ref;