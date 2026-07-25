DROP TABLE IF EXISTS document_upload_requests;

ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS ck_documents_parse_error;

ALTER TABLE documents
    DROP COLUMN IF EXISTS parsed_at,
    DROP COLUMN IF EXISTS parse_error;
