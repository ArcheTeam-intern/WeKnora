package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

// wikiRefTestStub is a Server seeded with the minimum services needed to
// exercise resolveWikiPageSourceRefs end-to-end. It does not depend on a
// database; each test supplies the chunks, the wiki page, the KB set and
// the in-scope knowledge ids the page's SourceRefs are expected to match.
type wikiRefTestStub struct {
	srv           *Server
	kb            *types.KnowledgeBase
	chunksByDoc   map[string][]*types.Chunk
	wikiPage      *types.WikiPage
	inScopeDocIDs map[string]struct{}
}

func newWikiRefTestStub(t *testing.T) *wikiRefTestStub {
	t.Helper()
	staging, _, _ := newTestStaging(t)
	srv := NewServer(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	kb := &types.KnowledgeBase{ID: kbA, TenantID: 7, Name: "kb-" + kbA}
	srv.kbService = &stubKBService{kbs: map[string]*types.KnowledgeBase{kb.ID: kb}}
	srv.kbShareService = &stubKBShareService{shared: map[string]types.OrgMemberRole{}}
	srv.tenantService = &stubTenantService{tenants: map[uint64]*types.Tenant{7: {ID: 7, Name: "Caller"}}}
	srv.uploadStagingSvc = staging
	return &wikiRefTestStub{
		srv:           srv,
		kb:            kb,
		chunksByDoc:   map[string][]*types.Chunk{},
		inScopeDocIDs: map[string]struct{}{},
	}
}

func (s *wikiRefTestStub) installChunkService() {
	repo := &wikiRefFakeChunkRepo{chunksByDoc: s.chunksByDoc}
	s.srv.chunkService = &wikiRefFakeChunkService{repo: repo}
}

func (s *wikiRefTestStub) installWikiService() {
	s.srv.wikiService = &wikiRefFakeWikiService{page: s.wikiPage, kb: s.kb}
}

func (s *wikiRefTestStub) installKnowledgeService() {
	s.srv.knowledgeService = &wikiRefFakeKnowledgeService{
		repo: &wikiRefFakeKnowledgeRepo{inScope: s.inScopeDocIDs},
	}
}

type wikiRefFakeChunkRepo struct {
	interfaces.ChunkRepository
	chunksByDoc map[string][]*types.Chunk
}

func (r *wikiRefFakeChunkRepo) ListChunksByKnowledgeID(_ context.Context, _ uint64, docID string) ([]*types.Chunk, error) {
	return r.chunksByDoc[docID], nil
}

type wikiRefFakeChunkService struct {
	interfaces.ChunkService
	repo interfaces.ChunkRepository
}

func (s *wikiRefFakeChunkService) GetRepository() interfaces.ChunkRepository { return s.repo }

type wikiRefFakeKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	inScope map[string]struct{}
}

func (r *wikiRefFakeKnowledgeRepo) GetKnowledgeByID(_ context.Context, _ uint64, id string) (*types.Knowledge, error) {
	if _, ok := r.inScope[id]; !ok {
		return nil, errors.New("knowledge not found: " + id)
	}
	return &types.Knowledge{ID: id, TenantID: 7, KnowledgeBaseID: kbA}, nil
}

func (r *wikiRefFakeKnowledgeRepo) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	if _, ok := r.inScope[id]; !ok {
		return nil, errors.New("knowledge not found: " + id)
	}
	return &types.Knowledge{ID: id, TenantID: 7, KnowledgeBaseID: kbA}, nil
}

type wikiRefFakeKnowledgeService struct {
	interfaces.KnowledgeService
	repo interfaces.KnowledgeRepository
}

func (s *wikiRefFakeKnowledgeService) GetRepository() interfaces.KnowledgeRepository { return s.repo }

func (s *wikiRefFakeKnowledgeService) GetKnowledgeByIDOnly(ctx context.Context, id string) (*types.Knowledge, error) {
	return s.repo.GetKnowledgeByIDOnly(ctx, id)
}

type wikiRefFakeWikiService struct {
	interfaces.WikiPageService
	page *types.WikiPage
	kb   *types.KnowledgeBase
}

func (s *wikiRefFakeWikiService) GetPageBySlug(_ context.Context, kbID, slug string) (*types.WikiPage, error) {
	if s.kb == nil || s.kb.ID != kbID || s.page == nil || s.page.Slug != slug {
		return nil, errors.New("wiki page not found")
	}
	return s.page, nil
}

func wikiRefCtx() context.Context {
	ep := &types.MCPEndpoint{ID: "ep-1", TenantID: 7, Enabled: true, RateLimitPerMinute: 1000}
	ctx := context.WithValue(context.Background(), types.MCPEndpointContextKey, ep)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(7))
	ctx = types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: "user-1"})
	ctx = types.WithCaller(ctx, types.Caller{TenantID: 7, UserID: "user-1", Role: types.TenantRoleAdmin})
	return ctx
}

// TestWikiSourceRefsLegacyIDOnlyReadsFirstReadableChunk documents the
// document-level behaviour: a legacy "uuid" ref (no chunk ids) is verified
// by loading the first chunk under the caller's KB; an empty KB id on the
// chunk (legacy rows) is also accepted because the page is itself scoped.
func TestWikiSourceRefsLegacyIDOnlyReadsFirstReadableChunk(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.chunksByDoc["d1"] = []*types.Chunk{
		{ID: "c1", TenantID: 7, KnowledgeID: "d1", KnowledgeBaseID: kbA, Content: "项目A 依赖 服务B"},
	}
	stub.wikiPage = &types.WikiPage{
		Slug: "entity/xiang-mu-a", KnowledgeBaseID: kbA,
		SourceRefs: types.StringArray{"d1"},
	}
	stub.inScopeDocIDs = map[string]struct{}{"d1": {}}
	stub.installChunkService()
	stub.installWikiService()
	stub.installKnowledgeService()

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %+v", refs)
	}
	if refs[0].KnowledgeID != "d1" || refs[0].KnowledgeBaseID != kbA {
		t.Fatalf("ref identity lost: %+v", refs[0])
	}
	if refs[0].ProvenanceStatus != "verified" {
		t.Fatalf("legacy ID-only ref should be verified, got %+v", refs[0])
	}
	if refs[0].Quote != "项目A 依赖 服务B" {
		t.Fatalf("quote should be the chunk content, got %q", refs[0].Quote)
	}
	if len(refs[0].ChunkIDs) != 1 || refs[0].ChunkIDs[0] != "c1" {
		t.Fatalf("ref should expose the chunk it verified, got %+v", refs[0])
	}
}

// TestWikiSourceRefsChunkIDsMapPerChunk pins the per-chunk contract: one V2
// entry per cited chunk id, each with its own status and quote. A missing
// chunk is unverified_source with no quote, never borrowed from a sibling.
func TestWikiSourceRefsChunkIDsMapPerChunk(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.chunksByDoc["d1"] = []*types.Chunk{
		{ID: "c1", TenantID: 7, KnowledgeID: "d1", KnowledgeBaseID: kbA, Content: "real quote 1"},
		{ID: "c2", TenantID: 7, KnowledgeID: "d1", KnowledgeBaseID: kbA, Content: "real quote 2"},
	}
	stub.wikiPage = &types.WikiPage{
		Slug: "entity/xiang-mu-a", KnowledgeBaseID: kbA,
		SourceRefs: types.StringArray{"d1/c1/missing"},
	}
	stub.inScopeDocIDs = map[string]struct{}{"d1": {}}
	stub.installChunkService()
	stub.installWikiService()
	stub.installKnowledgeService()

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs (c1 + missing), got %+v", refs)
	}
	if refs[0].ProvenanceStatus != "verified" || refs[0].Quote != "real quote 1" {
		t.Fatalf("c1 should be verified with its own quote, got %+v", refs[0])
	}
	if refs[1].ProvenanceStatus != "unverified_source" || refs[1].Quote != "" {
		t.Fatalf("missing chunk must be unverified_source with no quote, got %+v", refs[1])
	}
	if refs[1].ChunkIDs[0] != "missing" {
		t.Fatalf("ref must echo the cited chunk id, got %+v", refs[1])
	}
}

// TestWikiSourceRefsOutOfScopeDocIsUnverifiedAndSilent pins the privacy
// contract: a ref that points at a knowledge id the caller cannot see under
// this KB must NOT echo the id, must NOT have a quote, and must report
// unverified_source so clients can tell "we don't know" from "verified".
func TestWikiSourceRefsOutOfScopeDocIsUnverifiedAndSilent(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.wikiPage = &types.WikiPage{
		Slug: "entity/xiang-mu-a", KnowledgeBaseID: kbA,
		SourceRefs: types.StringArray{"private-doc"},
	}
	// inScopeDocIDs is empty: knowledgeInScope returns not-found.
	stub.installChunkService()
	stub.installWikiService()
	stub.installKnowledgeService()

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %+v", refs)
	}
	if refs[0].KnowledgeID != "" {
		t.Fatalf("private knowledge id must not leak, got %+v", refs[0])
	}
	if refs[0].ProvenanceStatus != "unverified_source" {
		t.Fatalf("out-of-scope doc must be unverified_source, got %+v", refs[0])
	}
	if refs[0].Quote != "" {
		t.Fatalf("out-of-scope doc must not carry a quote, got %+v", refs[0])
	}
}

// TestWikiSourceRefsCrossKBChunkIsUnverified pins the cross-KB guard: a
// chunk whose KnowledgeBaseID is NOT the caller's authorized KB must not
// produce a verified quote, even when the knowledge id is in scope.
func TestWikiSourceRefsCrossKBChunkIsUnverified(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.chunksByDoc["d1"] = []*types.Chunk{
		{ID: "c1", TenantID: 7, KnowledgeID: "d1", KnowledgeBaseID: "other-kb", Content: "private quote"},
	}
	stub.wikiPage = &types.WikiPage{
		Slug: "entity/xiang-mu-a", KnowledgeBaseID: kbA,
		SourceRefs: types.StringArray{"d1/c1"},
	}
	stub.inScopeDocIDs = map[string]struct{}{"d1": {}}
	stub.installChunkService()
	stub.installWikiService()
	stub.installKnowledgeService()

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %+v", refs)
	}
	if refs[0].ProvenanceStatus != "unverified_source" {
		t.Fatalf("cross-KB chunk must be unverified_source, got %+v", refs[0])
	}
	if refs[0].Quote != "" {
		t.Fatalf("cross-KB chunk must not leak its content, got %+v", refs[0])
	}
	if refs[0].KnowledgeBaseID != kbA {
		t.Fatalf("ref must echo the authorized KB, got %+v", refs[0])
	}
}

// TestWikiSourceRefsNoServiceFallsBackToUnverified covers the configuration
// where the chunk service is not wired (e.g. an endpoint running against a
// build that omits it). The page must still report a V2 view; missing
// service just means every ref is unverified_source.
func TestWikiSourceRefsNoServiceFallsBackToUnverified(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.wikiPage = &types.WikiPage{
		Slug: "entity/xiang-mu-a", KnowledgeBaseID: kbA,
		SourceRefs: types.StringArray{"d1/c1"},
	}
	stub.inScopeDocIDs = map[string]struct{}{"d1": {}}
	stub.installWikiService()
	stub.installKnowledgeService()
	// No chunkService wired.

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %+v", refs)
	}
	if refs[0].ProvenanceStatus != "unverified_source" {
		t.Fatalf("missing service must downgrade to unverified_source, got %+v", refs[0])
	}
	if refs[0].Quote != "" {
		t.Fatalf("missing service must not invent a quote, got %+v", refs[0])
	}
}

// TestWikiSourceRefsEmptyPageIsUnknown guards the synthesize case: a page
// that carries no SourceRefs at all emits a single unknown ref so clients
// see "this page has no source attribution" instead of nothing.
func TestWikiSourceRefsEmptyPageIsUnknown(t *testing.T) {
	stub := newWikiRefTestStub(t)
	stub.wikiPage = &types.WikiPage{
		Slug: "index", KnowledgeBaseID: kbA, SourceRefs: types.StringArray{},
	}
	stub.installChunkService()
	stub.installWikiService()
	stub.installKnowledgeService()

	refs := stub.srv.resolveWikiPageSourceRefs(wikiRefCtx(), stub.wikiPage, stub.kb)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %+v", refs)
	}
	if refs[0].ProvenanceStatus != "unknown" {
		t.Fatalf("empty SourceRefs should be unknown, got %+v", refs[0])
	}
}

// silenceUnused keeps the unused mcp import stable if the test body changes
// during refactors; other files in this package import it transitively.
var _ = mcp.NewToolResultText
