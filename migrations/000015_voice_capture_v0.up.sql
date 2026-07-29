-- ============================================================================
-- 000015 · 通用语音输入 · 录音与 STT 转写
--
-- Push-to-Talk 是聊天输入区的通用能力，不属于费曼子系统：语音只负责把用户这句话
-- 变成文本，之后走的就是和打字完全相同的一条链路。因此这张表既不引用练习尝试，
-- 也不引用知识点，只挂在 (user_id, session_id) 上。
--
-- 本迁移不产生任何掌握状态或掌握证据：一段录音被转写，只说明用户说了话。
--
-- 原始转写(raw_transcript)写入后不可变；用户实际发出的文本是聊天消息本身，
-- 两者通过一次性回填的 message_id 关联 —— 这就是「保留原始转写与更正版本、
-- 不静默覆盖历史」的落地方式，不需要额外的「转写确认表」再存第三份文字。
-- ============================================================================

CREATE TABLE voice_captures (
    capture_id          UUID         PRIMARY KEY,          -- 后端 UUIDv7
    user_id             VARCHAR(64)  NOT NULL,
    session_id          VARCHAR(64)  NOT NULL,

    status              VARCHAR(20)  NOT NULL,             -- uploaded/transcribing/transcribed/failed
    mime_type           VARCHAR(100) NOT NULL,
    size_bytes          INTEGER      NOT NULL,
    duration_ms         INTEGER,
    sha256              BYTEA        NOT NULL,
    audio_data          BYTEA        NOT NULL,

    stt_provider        VARCHAR(50),
    stt_model           VARCHAR(100),
    stt_request_id      VARCHAR(128),
    raw_transcript      TEXT,
    confidence          NUMERIC(4,3),                      -- 0-1；供应商不返回时为 NULL
    ambiguous_terms     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    needs_confirmation  BOOLEAN      NOT NULL DEFAULT TRUE,
    confirmation_reason VARCHAR(32)  NOT NULL DEFAULT '',
    transcript_error    TEXT,

    message_id          UUID,                              -- 发送后一次性回填；未发送保持 NULL
    created_by          VARCHAR(64)  NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT uk_voice_captures_owner UNIQUE (capture_id, user_id),
    -- 同一会话内重复提交完全相同的字节直接命中已有记录，不重复调用 STT、不重复计费。
    CONSTRAINT uk_voice_captures_dedupe UNIQUE (user_id, session_id, sha256),

    CONSTRAINT ck_voice_captures_status CHECK (status IN (
        'uploaded', 'transcribing', 'transcribed', 'failed')),
    CONSTRAINT ck_voice_captures_mime CHECK (btrim(mime_type) <> ''),
    CONSTRAINT ck_voice_captures_size CHECK (size_bytes > 0),
    CONSTRAINT ck_voice_captures_duration CHECK (duration_ms IS NULL OR duration_ms > 0),
    CONSTRAINT ck_voice_captures_sha256 CHECK (octet_length(sha256) = 32),
    CONSTRAINT ck_voice_captures_confidence CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    CONSTRAINT ck_voice_captures_ambiguous CHECK (jsonb_typeof(ambiguous_terms) = 'array'),
    CONSTRAINT ck_voice_captures_reason CHECK (confirmation_reason IN (
        '', 'transcribe_failed', 'missing_confidence', 'low_confidence', 'ambiguous_terms')),
    CONSTRAINT ck_voice_captures_terminal CHECK (
        (status = 'transcribed' AND raw_transcript IS NOT NULL AND transcript_error IS NULL)
        OR (status = 'failed' AND transcript_error IS NOT NULL AND raw_transcript IS NULL)
        OR (status IN ('uploaded', 'transcribing'))),
    -- 只有转写成功的录音才可能被真正发送成一条消息。
    CONSTRAINT ck_voice_captures_message CHECK (message_id IS NULL OR status = 'transcribed'),
    CONSTRAINT ck_voice_captures_created_owner CHECK (created_by = user_id),

    -- 复合外键：与 agent_turn_lease/feynman_practice_states 一致，
    -- 从数据库层杜绝把录音写进别人的会话。
    CONSTRAINT fk_voice_captures_session FOREIGN KEY (session_id, user_id)
        REFERENCES agent_memory_session(session_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_voice_captures_message FOREIGN KEY (message_id)
        REFERENCES agent_memory_episodic(message_id) ON DELETE RESTRICT,
    CONSTRAINT fk_voice_captures_created_by FOREIGN KEY (created_by)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_voice_captures_session
    ON voice_captures (user_id, session_id, created_at DESC);

COMMENT ON TABLE  voice_captures                     IS '通用语音输入的录音与 STT 转写结果；受控可变，只允许 transcribing→终态、message_id 一次性回填';
COMMENT ON COLUMN voice_captures.audio_data          IS 'v0 音频原始字节直接存 PostgreSQL；录音有大小/时长硬上限，暂不引入对象存储';
COMMENT ON COLUMN voice_captures.raw_transcript      IS 'STT 原始输出，写入后不可变；它是不可信输入，不得被当作可信内容或掌握证据';
COMMENT ON COLUMN voice_captures.confidence          IS 'STT 整体置信度 0-1；供应商不返回时为 NULL，服务层按「需要用户确认」处理';
COMMENT ON COLUMN voice_captures.ambiguous_terms     IS '命中术语词表的疑似误转写 [{"term":"幂等","heard":"密等"}]；启发式提示，不代表判定错误';
COMMENT ON COLUMN voice_captures.needs_confirmation  IS '本次是否要求用户先确认再发送；记录当时实际生效的裁决，便于回溯自动发送的决定';
COMMENT ON COLUMN voice_captures.message_id          IS '该转写最终被发送为哪条聊天消息；只能从 NULL 写一次，用于关联「原始转写」与「更正后文本」';

-- ---------------------------------------------------------------------------
-- 逐列守卫触发器。
--
-- 这里没有沿用 feynman_audio_tasks「一律禁止 UPDATE」的粗粒度写法：那种表只能在
-- 插入的一瞬间凑齐全部结果，中途崩溃就会留下永远卡在 transcribing 的僵尸行。
-- 改成逐列守卫后，既保住「原始转写不可变、绑定关系只能建立一次」，
-- 又允许同步 STT 的两段式写入和崩溃后的接管重试。
-- ---------------------------------------------------------------------------
CREATE FUNCTION guard_voice_capture_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'voice_captures rows cannot be deleted' USING ERRCODE = '55000';
    END IF;

    IF NEW.capture_id <> OLD.capture_id
       OR NEW.user_id <> OLD.user_id
       OR NEW.session_id <> OLD.session_id
       OR NEW.sha256 <> OLD.sha256
       OR NEW.audio_data <> OLD.audio_data
       OR NEW.mime_type <> OLD.mime_type
       OR NEW.size_bytes <> OLD.size_bytes
       OR NEW.created_by <> OLD.created_by
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'voice_captures identity and audio columns are immutable'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.status IN ('transcribed', 'failed') THEN
        IF NEW.status <> OLD.status
           OR NEW.raw_transcript IS DISTINCT FROM OLD.raw_transcript
           OR NEW.transcript_error IS DISTINCT FROM OLD.transcript_error
           OR NEW.confidence IS DISTINCT FROM OLD.confidence THEN
            RAISE EXCEPTION 'voice_captures transcript is immutable once terminal'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    IF OLD.message_id IS NOT NULL AND NEW.message_id IS DISTINCT FROM OLD.message_id THEN
        RAISE EXCEPTION 'voice_captures.message_id can only be set once'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_voice_captures_guard
    BEFORE UPDATE OR DELETE ON voice_captures
    FOR EACH ROW EXECUTE FUNCTION guard_voice_capture_mutation();

CREATE TRIGGER trg_voice_captures_updated_at
    BEFORE UPDATE ON voice_captures
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
