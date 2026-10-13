package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// allowedKnowledgeBases returns every knowledge base the endpoint may touch,
// each one re-authorized for the calling principal through access.ResolveKB so a
// stale or foreign ID in the allowlist is dropped rather than trusted. An
// unrestricted endpoint sees the workspace's own knowledge bases.
//
// The result is then narrowed by the caller's per-request selection, so the
// effective range is always
//
//	user authorization 鈭?endpoint range 鈭?request selection
//
// A selection naming an id outside that intersection is refused (not silently
// dropped): answering from a different range than the caller asked for would be
// a silent authorization change.
func (s *Server) allowedKnowledgeBases(
	ctx context.Context, ep *types.MCPEndpoint,
) ([]*types.KnowledgeBase, error) {
	allowed, err := s.endpointKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, err
	}
	return applySelection(allowed, RequestScopeOf(ctx))
}

// endpointKnowledgeBases is the endpoint-range, user-authorized set without the
// request selection applied. Kept separate because list_knowledge_bases is the
// discovery tool: it reports candidates before a selection exists.
func (s *Server) endpointKnowledgeBases(
	ctx context.Context, ep *types.MCPEndpoint,
) ([]*types.KnowledgeBase, error) {
	if !ep.RestrictsKnowledgeBases() {
		return s.kbService.ListKnowledgeBases(ctx)
	}
	out := make([]*types.KnowledgeBase, 0, len(ep.KnowledgeBaseIDs))
	for _, id := range ep.KnowledgeBaseIDs {
		kb, err := s.authorizedKnowledgeBase(ctx, id, types.OrgRoleViewer)
		if err != nil {
			logger.Warnf(ctx, "[mcpserver] endpoint %s references unavailable knowledge base %s: %v",
				ep.ID, id, err)
			continue
		}
		out = append(out, kb)
	}
	return out, nil
}

// authorizedKnowledgeBase loads a knowledge base by ID and resolves the
// caller's permission on it (ownership or organization share). The returned
// error is access.ErrForbidden / ErrNotFound for authorization failures.
func (s *Server) authorizedKnowledgeBase(
	ctx context.Context, kbID string, required types.OrgMemberRole,
) (*types.KnowledgeBase, error) {
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, strings.TrimSpace(kbID))
	if err != nil || kb == nil {
		return nil, access.ErrNotFound
	}
	if _, err := s.resolveKB(ctx, kb, required); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *Server) resolveKB(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (*access.KBAccess, error) {
	request := access.KBRequest{Caller: types.CallerFromContext(ctx)}
	return access.ResolveKB(ctx, request, kb, required, s.kbShareService, nil)
}

// scopedKBContext authorizes one knowledge base at the required permission
// and returns the context every document-level call must run under: it
// carries the resolved grant (so access.RequireKBWrite sees an explicit
// authorization instead of an implicit "same tenant") and switches the
// execution tenant to the knowledge base owner, which is what makes shared
// knowledge bases work. Their documents are stored under the owner tenant,
// so listing, reading and writing them through the caller's tenant would
// either find nothing or create rows under the wrong tenant.
//
// TenantInfo is swapped to the owner as well so model and storage settings
// resolve against the workspace that holds the data, mirroring the service
// layer's withKBWriteTenantInfo.
func (s *Server) scopedKBContext(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (context.Context, error) {
	_, scoped, err := s.authorizeKB(ctx, kb, required)
	if err != nil {
		if errors.Is(err, access.ErrForbidden) || errors.Is(err, access.ErrUnauthorized) {
			if required == types.OrgRoleViewer {
				return ctx, fmt.Errorf("this endpoint is not allowed to read knowledge base %q", kb.ID)
			}
			return ctx, fmt.Errorf("this endpoint is not allowed to write to knowledge base %q", kb.ID)
		}
		return ctx, err
	}
	return scoped, nil
}

// authorizeKB resolves the caller's grant on kb and returns it together with
// the owner-scoped context described on scopedKBContext.
func (s *Server) authorizeKB(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (*access.KBAccess, context.Context, error) {
	grant, err := s.resolveKB(ctx, kb, required)
	if err != nil {
		return nil, ctx, err
	}
	scoped := grant.Context(ctx)
	caller := types.CallerFromContext(ctx)
	if kb.TenantID != caller.TenantID && s.tenantService != nil {
		owner, err := s.tenantService.GetTenantByID(ctx, kb.TenantID)
		if err != nil || owner == nil {
			return nil, ctx, fmt.Errorf("the workspace owning knowledge base %q is unavailable", kb.ID)
		}
		scoped = context.WithValue(scoped, types.TenantInfoContextKey, owner)
	}
	return grant, scoped, nil
}

// selectKnowledgeBases narrows the allowed set to the ones a caller named,
// accepting either IDs or exact (case-insensitive) names. With no selector the
// frozen request selection (or the whole allowed set when none was sent) is
// returned.
func (s *Server) selectKnowledgeBases(
	ctx context.Context, ep *types.MCPEndpoint, requested []string,
) ([]*types.KnowledgeBase, error) {
	allowed, err := s.allowedKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return nil, &ToolError{
			Code:    CodeKBOutOfScope,
			Message: "no knowledge base is available to this caller on this endpoint",
		}
	}
	cleaned := make([]string, 0, len(requested))
	for _, r := range requested {
		if r = strings.TrimSpace(r); r != "" {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) == 0 {
		return allowed, nil
	}
	out := make([]*types.KnowledgeBase, 0, len(cleaned))
	seen := map[string]struct{}{}
	for _, sel := range cleaned {
		kb := matchKnowledgeBase(allowed, sel)
		if kb == nil {
			// Naming a knowledge base outside the frozen range is a scope
			// violation, not a missing resource: the caller may well be able to
			// see it elsewhere, but not on this request.
			return nil, outOfScope(
				"knowledge base %q is outside the selected range for this request; "+
					"call list_knowledge_bases to see what is available", sel)
		}
		if _, dup := seen[kb.ID]; dup {
			continue
		}
		seen[kb.ID] = struct{}{}
		out = append(out, kb)
	}
	return out, nil
}

func matchKnowledgeBase(kbs []*types.KnowledgeBase, selector string) *types.KnowledgeBase {
	for _, kb := range kbs {
		if kb.ID == selector {
			return kb
		}
	}
	for _, kb := range kbs {
		if strings.EqualFold(strings.TrimSpace(kb.Name), selector) {
			return kb
		}
	}
	return nil
}

// searchTargetsFor builds whole-knowledge-base search targets. TenantID is
// carried per target because shared knowledge bases index under their owner.
func searchTargetsFor(kbs []*types.KnowledgeBase) types.SearchTargets {
	targets := make(types.SearchTargets, 0, len(kbs))
	for _, kb := range kbs {
		targets = append(targets, &types.SearchTarget{
			Type:            types.SearchTargetTypeKnowledgeBase,
			KnowledgeBaseID: kb.ID,
			TenantID:        kb.TenantID,
		})
	}
	return targets
}

func knowledgeBaseIDs(kbs []*types.KnowledgeBase) []string {
	ids := make([]string, 0, len(kbs))
	for _, kb := range kbs {
		ids = append(ids, kb.ID)
	}
	return ids
}

// retrievableKnowledgeBases keeps knowledge bases that have a vector or
// keyword index, i.e. the ones the search tools can actually query.
func retrievableKnowledgeBases(kbs []*types.KnowledgeBase) []*types.KnowledgeBase {
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if kb.IsVectorEnabled() || kb.IsKeywordEnabled() {
			out = append(out, kb)
		}
	}
	return out
}

// wikiKnowledgeBases keeps knowledge bases with a generated wiki.
func wikiKnowledgeBases(kbs []*types.KnowledgeBase) []*types.KnowledgeBase {
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if kb.IsWikiEnabled() {
			out = append(out, kb)
		}
	}
	return out
}

func wikiScopesFor(kbs []*types.KnowledgeBase) []tools.WikiScope {
	return tools.NewWikiScopesFromKBIDs(knowledgeBaseIDs(kbs))
}

// knowledgeInScope loads a document and confirms it belongs to a knowledge base
// the endpoint may touch AND that is inside the frozen request selection. The
// lookup is tenant-agnostic because a shared knowledge base's documents live
// under the owner tenant; the document must then match an allowed knowledge base
// and that knowledge base's tenant.
//
// This is the by-id re-check the plan requires: narrowing must not be bypassable
// by naming a document directly instead of searching for it.
func (s *Server) knowledgeInScope(
	ctx context.Context, ep *types.MCPEndpoint, knowledgeID string,
) (*types.Knowledge, *types.KnowledgeBase, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return nil, nil, invalidArgument("knowledge_id is required")
	}
	k, err := s.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil || k == nil {
		return nil, nil, notFound("document %q was not found", knowledgeID)
	}
	if scope := RequestScopeOf(ctx); scope.Narrows() && !scope.Contains(k.KnowledgeBaseID) {
		return nil, nil, outOfScope(
			"document %q belongs to a knowledge base outside the selected range for this request",
			knowledgeID)
	}
	allowed, err := s.allowedKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, nil, err
	}
	kb := matchKnowledgeBase(allowed, k.KnowledgeBaseID)
	if kb == nil || kb.TenantID != k.TenantID {
		return nil, nil, outOfScope("document %q is outside this endpoint's scope", knowledgeID)
	}
	return k, kb, nil
}

// knowledgeBaseInScope re-checks a knowledge base named directly (knowledge base
// status, wiki tools) against the frozen selection before any expensive work.
func (s *Server) knowledgeBaseInScope(
	ctx context.Context, selector string,
) (*types.KnowledgeBase, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return nil, &ToolError{Code: CodeUnauthorized, Message: "unauthorized"}
	}
	kbs, err := s.selectKnowledgeBases(ctx, ep, []string{selector})
	if err != nil {
		return nil, err
	}
	if len(kbs) == 0 {
		return nil, outOfScope("knowledge base %q is outside the selected range", selector)
	}
	return kbs[0], nil
}
