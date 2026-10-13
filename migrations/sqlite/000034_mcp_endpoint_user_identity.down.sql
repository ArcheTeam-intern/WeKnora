-- Rollback of 000034_mcp_endpoint_user_identity.
-- SQLite supports DROP COLUMN from 3.35; the bundled driver is newer.

ALTER TABLE mcp_endpoints
    DROP COLUMN require_user_identity;
