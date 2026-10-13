-- Migration: 000116_mcp_uploads
-- Persisted upload receipts for the MCP integration's binary upload channel.
--
-- Why this lives in WeKnora's database rather than in the thin client:
--   * the receipt must survive the 24h byte cleanup so a retried import can be
--     answered with the document id it already produced (72h retention);
--   * it must survive a service restart so a crashed import can be recovered
--     without creating a duplicate document;
--   * the caller identity that owns an upload is part of its key, so uploads
--     cannot be resolved across users, workspaces or endpoints.

DO $$ BEGIN RAISE NOTICE '[Migration 000116] Creating mcp_uploads table'; END $$;

CREATE TABLE IF NOT EXISTS mcp_uploads (
    id                          VARCHAR(36) PRIMARY KEY,
    endpoint_id                 VARCHAR(36) NOT NULL,
    caller_workspace_id         BIGINT NOT NULL,
    caller_principal_storage_id VARCHAR(128) NOT NULL,
    owner_tenant_id             BIGINT NOT NULL,
    knowledge_base_id           VARCHAR(36) NOT NULL,
    idempotency_key             VARCHAR(64) NOT NULL DEFAULT '',
    file_name                   VARCHAR(255) NOT NULL,
    file_size                   BIGINT NOT NULL,
    sha256                      VARCHAR(64) NOT NULL DEFAULT '',
    state                       VARCHAR(16) NOT NULL,
    document_id                 VARCHAR(36) NOT NULL DEFAULT '',
    deduplicated                BOOLEAN NOT NULL DEFAULT false,
    queued                      BOOLEAN NOT NULL DEFAULT false,
    last_error_code             VARCHAR(48) NOT NULL DEFAULT '',
    last_error_message          TEXT NOT NULL DEFAULT '',
    lease_token                 VARCHAR(36) NOT NULL DEFAULT '',
    lease_expires_at            TIMESTAMP WITH TIME ZONE,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at                  TIMESTAMP WITH TIME ZONE NOT NULL,
    receipt_expires_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Lookup path: (id + full caller identity + endpoint).
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_scope
    ON mcp_uploads (endpoint_id, caller_workspace_id, caller_principal_storage_id);

CREATE INDEX IF NOT EXISTS idx_mcp_uploads_kb ON mcp_uploads (knowledge_base_id);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_state ON mcp_uploads (state);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_expires ON mcp_uploads (expires_at);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_receipt_expires ON mcp_uploads (receipt_expires_at);

-- Request idempotency: one key per endpoint + caller workspace + caller
-- principal. Scoped to non-empty keys so an upload that sends no
-- Idempotency-Key is not forced to share a single empty-string row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_uploads_idempotency
    ON mcp_uploads (endpoint_id, caller_workspace_id, caller_principal_storage_id, idempotency_key)
    WHERE idempotency_key <> '';

COMMENT ON TABLE mcp_uploads IS
    'Staged uploads and their import receipts for the MCP upload transport; bytes live at most 24h, receipts 72h';
COMMENT ON COLUMN mcp_uploads.caller_principal_storage_id IS
    'principal.StorageID() of the ORIGINAL caller (type:id), hashed for the filesystem layout; never the owner tenant';
COMMENT ON COLUMN mcp_uploads.owner_tenant_id IS
    'Workspace that owns the target knowledge base; used only for the resource-owner execution context';
COMMENT ON COLUMN mcp_uploads.lease_token IS
    'Import lease owner; every receipt mutation is conditional on this token so a stale attempt cannot overwrite a newer receipt';
COMMENT ON COLUMN mcp_uploads.idempotency_key IS
    'Client Idempotency-Key (UUID) for one user upload action; a retry of the same action reuses it';
