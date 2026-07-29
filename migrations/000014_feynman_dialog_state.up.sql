-- ============================================================================
-- 000014 · 对话式费曼学习 · 会话状态
-- 费曼学习不是独立页面，而是普通对话里的一种持续状态。这张表只保存
-- 「当前这个会话处在练习的哪一步」，是当前投影而不是历史账本：
-- 每一轮的提问与回答本身就是真实聊天消息，历史留痕在 agent_memory_episodic。
--
-- 本迁移不产生任何掌握状态或掌握证据：练习状态只影响下一条消息如何被解释。
-- ============================================================================

CREATE TABLE feynman_practice_states (
    session_id               VARCHAR(64)  PRIMARY KEY,
    user_id                  VARCHAR(64)  NOT NULL,
    state                    VARCHAR(24)  NOT NULL,
    -- 当前题目/主题原文：用户自述主题或 AI 上一轮生成的追问。
    -- v0 直接答题不依赖正式知识点，也不依赖文档供题，所以这里是自由文本而非外键。
    active_question_text     TEXT         NOT NULL DEFAULT '',
    question_origin          VARCHAR(16)  NOT NULL DEFAULT '',
    -- 最近一次真正送去分析的用户消息；配合 last_feedback 实现「同一条消息重试时回放」，
    -- 避免 assistant 落库失败后重试导致重复分析、重复推进状态。
    last_answered_message_id UUID,
    last_feedback            TEXT         NOT NULL DEFAULT '',
    round_no                 INT          NOT NULL DEFAULT 0,

    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT ck_feynman_practice_states_state CHECK (state IN (
        'idle', 'awaiting_topic', 'awaiting_answer', 'analyzing_answer',
        'awaiting_follow_up', 'queue_paused')),
    CONSTRAINT ck_feynman_practice_states_origin CHECK (question_origin IN ('', 'user_topic', 'ai_follow_up')),
    CONSTRAINT ck_feynman_practice_states_round CHECK (round_no >= 0),
    -- awaiting_answer 表示「AI 已经提出了一个明确的题目」，题目为空就无从判定下一条消息答的是什么。
    CONSTRAINT ck_feynman_practice_states_question CHECK (
        state <> 'awaiting_answer' OR btrim(active_question_text) <> ''),
    CONSTRAINT ck_feynman_practice_states_feedback CHECK (
        last_feedback = '' OR last_answered_message_id IS NOT NULL),
    -- 复合外键：与 agent_turn_lease 一致，从数据库层杜绝跨用户写入他人会话状态。
    CONSTRAINT fk_feynman_practice_states_session FOREIGN KEY (session_id, user_id)
        REFERENCES agent_memory_session(session_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_feynman_practice_states_message FOREIGN KEY (last_answered_message_id)
        REFERENCES agent_memory_episodic(message_id) ON DELETE RESTRICT
);

CREATE INDEX idx_feynman_practice_states_user_updated
    ON feynman_practice_states (user_id, updated_at DESC);

CREATE TRIGGER trg_feynman_practice_states_updated_at
    BEFORE UPDATE ON feynman_practice_states
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  feynman_practice_states                          IS '会话级费曼练习状态的当前投影；历史留痕在聊天消息中，本表不做 Append-only';
COMMENT ON COLUMN feynman_practice_states.state                    IS 'idle/awaiting_topic/awaiting_answer/analyzing_answer/awaiting_follow_up/queue_paused';
COMMENT ON COLUMN feynman_practice_states.active_question_text     IS '当前题目原文；用户自述主题或 AI 追问，不引用正式知识点';
COMMENT ON COLUMN feynman_practice_states.question_origin          IS 'user_topic=用户自述主题；ai_follow_up=AI 上一轮追问';
COMMENT ON COLUMN feynman_practice_states.last_answered_message_id IS '最近一次进入分析的用户消息；同一条消息重试时用于回放，不重复调用模型';
COMMENT ON COLUMN feynman_practice_states.last_feedback            IS '该消息对应的反馈全文；等 feynman_answers 落地后可降级为指针';
