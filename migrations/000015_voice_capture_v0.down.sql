DROP TRIGGER IF EXISTS trg_voice_captures_updated_at ON voice_captures;
DROP TRIGGER IF EXISTS trg_voice_captures_guard ON voice_captures;
DROP FUNCTION IF EXISTS guard_voice_capture_mutation();
DROP TABLE IF EXISTS voice_captures;
