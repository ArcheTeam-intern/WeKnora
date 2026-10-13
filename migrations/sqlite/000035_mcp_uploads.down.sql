-- Rollback of 000035_mcp_uploads.
--
-- Drops upload RECEIPTS only: no documents, chunks, indexes, wikis or graph data
-- are touched, and no file the deployment still references is deleted.

DROP INDEX IF EXISTS idx_mcp_uploads_idempotency;
DROP INDEX IF EXISTS idx_mcp_uploads_receipt_expires;
DROP INDEX IF EXISTS idx_mcp_uploads_expires;
DROP INDEX IF EXISTS idx_mcp_uploads_state;
DROP INDEX IF EXISTS idx_mcp_uploads_kb;
DROP INDEX IF EXISTS idx_mcp_uploads_scope;
DROP TABLE IF EXISTS mcp_uploads;
