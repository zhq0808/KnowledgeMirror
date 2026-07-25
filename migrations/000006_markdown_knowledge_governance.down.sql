-- 000006 回滚：按依赖反序删除。DROP TABLE 会同时删除表上的 trigger。
DROP TABLE IF EXISTS content_candidate_sources;
DROP TABLE IF EXISTS content_candidates;
DROP TABLE IF EXISTS knowledge_point_sources;
DROP TABLE IF EXISTS knowledge_points;
DROP TABLE IF EXISTS source_chunks;
DROP TABLE IF EXISTS document_usages;
ALTER TABLE IF EXISTS documents DROP CONSTRAINT IF EXISTS fk_documents_current_version;
DROP TABLE IF EXISTS document_versions;
DROP TABLE IF EXISTS documents;

DROP FUNCTION IF EXISTS protect_source_chunk_lineage();
DROP FUNCTION IF EXISTS enforce_archive_only_usage();
DROP FUNCTION IF EXISTS reject_document_version_mutation();