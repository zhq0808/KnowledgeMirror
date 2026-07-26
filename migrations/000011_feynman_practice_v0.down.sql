-- 回滚 000011：删除语音费曼练习相关表，以及知识点上新增的 Rubric 指针列。
-- 知识点主表本身不删除，只回收本迁移新增的列。

ALTER TABLE knowledge_points DROP CONSTRAINT IF EXISTS fk_knowledge_points_current_rubric;
ALTER TABLE knowledge_points DROP COLUMN IF EXISTS current_rubric_version_id;

DROP TRIGGER IF EXISTS trg_knowledge_point_rubrics_append_only ON knowledge_point_rubrics;
DROP FUNCTION IF EXISTS reject_knowledge_point_rubric_mutation();
DROP TABLE IF EXISTS knowledge_point_rubrics;

ALTER TABLE feynman_attempts DROP CONSTRAINT IF EXISTS fk_feynman_attempts_confirmation;
DROP TRIGGER IF EXISTS trg_feynman_confirmations_append_only ON feynman_transcript_confirmations;
DROP FUNCTION IF EXISTS reject_feynman_confirmation_mutation();
DROP TABLE IF EXISTS feynman_transcript_confirmations;

ALTER TABLE feynman_attempts DROP CONSTRAINT IF EXISTS fk_feynman_attempts_active_audio;
DROP TRIGGER IF EXISTS trg_feynman_audio_tasks_append_only ON feynman_audio_tasks;
DROP FUNCTION IF EXISTS reject_feynman_audio_task_mutation();
DROP TABLE IF EXISTS feynman_audio_tasks;

DROP TRIGGER IF EXISTS trg_feynman_attempts_guard ON feynman_attempts;
DROP FUNCTION IF EXISTS enforce_feynman_attempt_guard();
DROP TRIGGER IF EXISTS trg_feynman_attempts_updated_at ON feynman_attempts;
DROP TABLE IF EXISTS feynman_attempts;
