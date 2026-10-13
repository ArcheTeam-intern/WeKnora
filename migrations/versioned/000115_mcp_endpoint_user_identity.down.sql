-- Rollback of 000115_mcp_endpoint_user_identity.

ALTER TABLE mcp_endpoints
    DROP COLUMN IF EXISTS require_user_identity;
