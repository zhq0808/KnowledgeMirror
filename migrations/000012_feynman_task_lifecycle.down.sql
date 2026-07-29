DROP TRIGGER IF EXISTS trg_feynman_audio_tasks_no_delete ON feynman_audio_tasks;
DROP FUNCTION IF EXISTS reject_feynman_audio_task_delete();
DROP TRIGGER IF EXISTS trg_feynman_audio_tasks_record_event ON feynman_audio_tasks;
DROP FUNCTION IF EXISTS record_feynman_audio_task_event();
DROP TRIGGER IF EXISTS trg_feynman_audio_tasks_transition_guard ON feynman_audio_tasks;
DROP FUNCTION IF EXISTS guard_feynman_audio_task_transition();
DROP TABLE IF EXISTS feynman_audio_task_events;

ALTER TABLE feynman_audio_tasks DROP COLUMN IF EXISTS updated_at;

CREATE FUNCTION reject_feynman_audio_task_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'feynman_audio_tasks are append-only'
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_feynman_audio_tasks_append_only
    BEFORE UPDATE OR DELETE ON feynman_audio_tasks
    FOR EACH ROW EXECUTE FUNCTION reject_feynman_audio_task_mutation();