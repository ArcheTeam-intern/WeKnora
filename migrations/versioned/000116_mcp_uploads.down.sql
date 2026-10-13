-- Rollback of 000116_mcp_uploads.
--
-- This drops upload RECEIPTS only. It does not touch documents, chunks, indexes,
-- wikis or graph data, and it does not delete any file the deployment still
-- references.

DROP INDEX IF EXISTS idx_mcp_uploads_idempotency;
DROP INDEX IF EXISTS idx_mcp_uploads_receipt_expires;
DROP INDEX IF EXISTS idx_mcp_uploads_expires;
DROP INDEX IF EXISTS idx_mcp_uploads_state;
DROP INDEX IF EXISTS idx_mcp_uploads_kb;
DROP INDEX IF EXISTS idx_mcp_uploads_scope;
DROP TABLE IF EXISTS mcp_uploads;
