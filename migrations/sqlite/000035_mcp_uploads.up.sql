-- Mirrors versioned migration 000116_mcp_uploads:
-- persisted upload receipts for the MCP integration's binary upload channel.
--
-- SQLite notes:
--   * BOOLEAN is stored as INTEGER, matching every other flag column.
--   * A partial unique index (WHERE idempotency_key <> '') is supported from
--     SQLite 3.8.0, which the bundled driver is far newer than.

CREATE TABLE IF NOT EXISTS mcp_uploads (
    id                          VARCHAR(36) PRIMARY KEY,
    endpoint_id                 VARCHAR(36) NOT NULL,
    caller_workspace_id         INTEGER NOT NULL,
    caller_principal_storage_id VARCHAR(128) NOT NULL,
    owner_tenant_id             INTEGER NOT NULL,
    knowledge_base_id           VARCHAR(36) NOT NULL,
    idempotency_key             VARCHAR(64) NOT NULL DEFAULT '',
    file_name                   VARCHAR(255) NOT NULL,
    file_size                   INTEGER NOT NULL,
    sha256                      VARCHAR(64) NOT NULL DEFAULT '',
    state                       VARCHAR(16) NOT NULL,
    document_id                 VARCHAR(36) NOT NULL DEFAULT '',
    deduplicated                INTEGER NOT NULL DEFAULT 0,
    queued                      INTEGER NOT NULL DEFAULT 0,
    last_error_code             VARCHAR(48) NOT NULL DEFAULT '',
    last_error_message          TEXT NOT NULL DEFAULT '',
    lease_token                 VARCHAR(36) NOT NULL DEFAULT '',
    lease_expires_at            DATETIME,
    created_at                  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at                  DATETIME NOT NULL,
    receipt_expires_at          DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mcp_uploads_scope
    ON mcp_uploads (endpoint_id, caller_workspace_id, caller_principal_storage_id);

CREATE INDEX IF NOT EXISTS idx_mcp_uploads_kb ON mcp_uploads (knowledge_base_id);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_state ON mcp_uploads (state);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_expires ON mcp_uploads (expires_at);
CREATE INDEX IF NOT EXISTS idx_mcp_uploads_receipt_expires ON mcp_uploads (receipt_expires_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_uploads_idempotency
    ON mcp_uploads (endpoint_id, caller_workspace_id, caller_principal_storage_id, idempotency_key)
    WHERE idempotency_key <> '';
