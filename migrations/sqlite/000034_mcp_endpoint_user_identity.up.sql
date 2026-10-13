-- Mirrors versioned migration 000115_mcp_endpoint_user_identity:
-- per-endpoint switch that requires a real WeKnora user JWT on the MCP
-- surface. SQLite has no ADD COLUMN IF NOT EXISTS before 3.35 and the
-- migration runner applies each file once, so a plain ALTER is used.

ALTER TABLE mcp_endpoints
    ADD COLUMN require_user_identity INTEGER NOT NULL DEFAULT 0;
