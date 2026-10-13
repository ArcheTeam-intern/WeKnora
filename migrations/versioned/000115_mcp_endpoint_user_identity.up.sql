-- Migration: 000115_mcp_endpoint_user_identity
-- Adds the per-endpoint switch that turns an MCP endpoint into a user-scoped
-- surface: when true, callers must present a real WeKnora user JWT in
-- X-WeKnora-User-Token in addition to the endpoint bearer token.
--
-- Default false keeps every pre-existing endpoint on the historical machine
-- identity, so already-connected MCP clients are unaffected.

DO $$ BEGIN RAISE NOTICE '[Migration 000115] Adding mcp_endpoints.require_user_identity'; END $$;

ALTER TABLE mcp_endpoints
    ADD COLUMN IF NOT EXISTS require_user_identity BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN mcp_endpoints.require_user_identity IS
    'When true the endpoint requires a WeKnora user JWT (X-WeKnora-User-Token) and authorizes calls as that user instead of the endpoint machine identity';
