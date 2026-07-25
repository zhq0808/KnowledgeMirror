-- ============================================================================
-- 000007 · 资料上传入口：解析失败原因 + 上传幂等键
-- 版本创建与片段解析分两个事务：解析失败时资料留在 failed 状态，可重试。
-- 幂等键保证同一次上传动作重复提交不会产生第二个版本。
-- ============================================================================

ALTER TABLE documents
    ADD COLUMN parse_error TEXT,
    ADD COLUMN parsed_at   TIMESTAMPTZ;

ALTER TABLE documents
    ADD CONSTRAINT ck_documents_parse_error
    CHECK (parse_error IS NULL OR btrim(parse_error) <> '');

COMMENT ON COLUMN documents.parse_error IS '最近一次解析失败的原因；解析成功后清空';
COMMENT ON COLUMN documents.parsed_at IS '当前版本最近一次解析成功时间；解析不改变任何掌握状态';

-- ---------------------------------------------------------------------------
-- 上传幂等键：同一用户同一 idempotency_key 只允许对应一个资料版本。
-- request_hash 是本次上传原始字节的 SHA-256，用于识别“同键不同内容”的错误重放。
-- ---------------------------------------------------------------------------
CREATE TABLE document_upload_requests (
    user_id          VARCHAR(64)  NOT NULL,
    idempotency_key  VARCHAR(128) NOT NULL,
    request_hash     BYTEA        NOT NULL,
    document_id      UUID         NOT NULL,
    version_id       UUID         NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT pk_document_upload_requests PRIMARY KEY (user_id, idempotency_key),
    CONSTRAINT ck_document_upload_requests_key CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT ck_document_upload_requests_hash CHECK (octet_length(request_hash) = 32),
    CONSTRAINT fk_document_upload_requests_version FOREIGN KEY (version_id, document_id, user_id)
        REFERENCES document_versions(version_id, document_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_document_upload_requests_user FOREIGN KEY (user_id)
        REFERENCES agent_user(user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_document_upload_requests_created
    ON document_upload_requests (created_at);

COMMENT ON TABLE document_upload_requests IS '资料上传幂等记录；重复提交同一幂等键返回首次结果，不新建版本';
