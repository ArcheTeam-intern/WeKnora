package service

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// resolvingCatalog answers Resolve from a fixed handle → tenant map and records
// every Bind, which is all bindContentResources exercises.
// Helpers carried over from the deleted artifact collector tests; the
// knowledge content-binding and resource-release tests still use them.

type bindCall struct {
	ref       string
	ownerType string
	ownerID   string
	relation  string
}

// fakeCatalog captures Bind calls; every other ResourceCatalog method is a
// no-op stub because the collector only ever calls Bind.

type fakeCatalog struct {
	binds   []bindCall
	bindErr error
	// Release scripting, used by the deletion-guard tests. releaseRemaining
	// maps a reference to the binding count left after the release; a missing
	// entry answers -1 ("not a catalog handle").
	releaseRemaining map[string]int64
	releaseErr       error
	releases         []string
	ownerRefs        map[string][]string
	listErr          error
}

func (c *fakeCatalog) Register(context.Context, uint64, string, interfaces.ResourceRegistration) (string, error) {
	return "", nil
}

func (c *fakeCatalog) Resolve(context.Context, string) (*types.StoredResource, error) {
	return nil, nil
}

func (c *fakeCatalog) ResolvePath(_ context.Context, v string) (string, *types.StoredResource, error) {
	return v, nil, nil
}

func (c *fakeCatalog) Bind(_ context.Context, ref, ownerType, ownerID, relation string) error {
	c.binds = append(c.binds, bindCall{ref, ownerType, ownerID, relation})
	return c.bindErr
}
func (c *fakeCatalog) MarkDeleted(context.Context, string) error { return nil }

func (c *fakeCatalog) Release(_ context.Context, ref, ownerType, ownerID string) (int64, error) {
	c.releases = append(c.releases, ref+"|"+ownerType+"|"+ownerID)
	if c.releaseErr != nil {
		return -1, c.releaseErr
	}
	if remaining, ok := c.releaseRemaining[ref]; ok {
		return remaining, nil
	}
	return -1, nil
}

func (c *fakeCatalog) ListReferencesByOwner(_ context.Context, _ string, ownerIDs ...string) ([]string, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []string
	for _, id := range ownerIDs {
		out = append(out, c.ownerRefs[id]...)
	}
	return out, nil
}

var _ interfaces.ResourceCatalog = (*fakeCatalog)(nil)

func (c *fakeCatalog) CreateAccessGrant(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (c *fakeCatalog) ResolveAccessGrant(context.Context, string) (*types.StoredResource, error) {
	return nil, nil
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// resourceRefFileService is a fileService whose SaveBytes returns a valid
// resource:// reference, so the collector's binding path is exercised.

type resolvingCatalog struct {
	tenantByRef map[string]uint64
	binds       []bindCall
}

func (c *resolvingCatalog) Resolve(_ context.Context, ref string) (*types.StoredResource, error) {
	tenantID, ok := c.tenantByRef[ref]
	if !ok {
		return nil, errors.New("resource not found")
	}
	handle, _ := types.ParseResourcePath(ref)
	return &types.StoredResource{ID: handle, Handle: handle, TenantID: tenantID}, nil
}

func (c *resolvingCatalog) Bind(_ context.Context, ref, ownerType, ownerID, relation string) error {
	c.binds = append(c.binds, bindCall{ref, ownerType, ownerID, relation})
	return nil
}

func (c *resolvingCatalog) Register(
	context.Context, uint64, string, interfaces.ResourceRegistration,
) (string, error) {
	return "", errors.New("unused")
}

func (c *resolvingCatalog) ResolvePath(
	_ context.Context, value string,
) (string, *types.StoredResource, error) {
	return value, nil, nil
}

func (c *resolvingCatalog) Release(context.Context, string, string, string) (int64, error) {
	return -1, nil
}

func (c *resolvingCatalog) ListReferencesByOwner(context.Context, string, ...string) ([]string, error) {
	return nil, nil
}

func (c *resolvingCatalog) MarkDeleted(context.Context, string) error { return nil }

func (c *resolvingCatalog) CreateAccessGrant(
	context.Context, string, time.Duration,
) (string, error) {
	return "", errors.New("unused")
}

func (c *resolvingCatalog) ResolveAccessGrant(
	context.Context, string,
) (*types.StoredResource, error) {
	return nil, errors.New("unused")
}

func contentRef(char string) string {
	return types.BuildResourcePath(strings.Repeat(char, types.ResourceHandleLength))
}

// Saving a chat answer into the knowledge base must claim the files it shows
// without copying them: the assistant message keeps its own claim, so either
// side can be deleted without breaking the other.
func TestBindContentResourcesClaimsEveryReferencedFile(t *testing.T) {
	chart := contentRef("a")
	table := contentRef("b")
	catalog := &resolvingCatalog{tenantByRef: map[string]uint64{chart: 7, table: 7}}
	svc := &knowledgeService{resourceCatalog: catalog}

	content := "## 结论\n\n![评分](" + chart + ")\n\n数据见 [表格](" + table + ")"
	svc.bindContentResources(context.Background(), 7, "kn-1", content)

	if len(catalog.binds) != 2 {
		t.Fatalf("binds = %v, want one per reference", catalog.binds)
	}
	for i, want := range []string{chart, table} {
		got := catalog.binds[i]
		if got.ref != want {
			t.Fatalf("binds[%d].ref = %q, want %q", i, got.ref, want)
		}
		if got.ownerType != types.ResourceOwnerKnowledge || got.ownerID != "kn-1" {
			t.Fatalf("binds[%d] owner = (%q, %q), want (%q, kn-1)",
				i, got.ownerType, got.ownerID, types.ResourceOwnerKnowledge)
		}
		if got.relation != types.ResourceRelationAttachment {
			t.Fatalf("binds[%d].relation = %q, want %q",
				i, got.relation, types.ResourceRelationAttachment)
		}
	}
}

// A handle pasted from another workspace must not be claimed: binding it would
// hand the caller a file it may not read.
func TestBindContentResourcesSkipsForeignAndUnknownHandles(t *testing.T) {
	mine := contentRef("c")
	theirs := contentRef("d")
	unknown := contentRef("e")
	catalog := &resolvingCatalog{tenantByRef: map[string]uint64{mine: 7, theirs: 9}}
	svc := &knowledgeService{resourceCatalog: catalog}

	content := "![a](" + mine + ") ![b](" + theirs + ") ![c](" + unknown + ")"
	svc.bindContentResources(context.Background(), 7, "kn-1", content)

	if len(catalog.binds) != 1 || catalog.binds[0].ref != mine {
		t.Fatalf("binds = %v, want only %q", catalog.binds, mine)
	}
}

func TestBindContentResourcesIsInertWithoutCatalog(t *testing.T) {
	svc := &knowledgeService{}
	// Must not panic; a deployment without a resource registry has nothing to
	// claim and keeps the pre-binding behaviour.
	svc.bindContentResources(context.Background(), 7, "kn-1", "![a]("+contentRef("f")+")")
}

func TestBindChunkResourcesClaimsContentAndImageInfo(t *testing.T) {
	fromContent := contentRef("p")
	fromInfo := contentRef("q")
	catalog := &resolvingCatalog{tenantByRef: map[string]uint64{fromContent: 7, fromInfo: 7}}
	svc := &knowledgeService{resourceCatalog: catalog}

	svc.bindChunkResources(context.Background(), 7, "kn-clone", []*types.Chunk{
		{Content: "![a](" + fromContent + ")"},
		{ImageInfo: `[{"url":"` + fromInfo + `"}]`},
	})

	if len(catalog.binds) != 2 {
		t.Fatalf("binds = %v, want content and image_info handles", catalog.binds)
	}
}

// fakeFileService is the minimal FileService the content-binding and resource
// release tests need; it came from the deleted artifact collector tests.
type fakeFileService struct {
	saved     map[string][]byte
	saveErr   error
	seq       int
	tenantIDs []uint64
}

func (f *fakeFileService) CheckConnectivity(_ context.Context) error { return nil }

func (f *fakeFileService) SaveFile(_ context.Context, _ *multipart.FileHeader, _ uint64, _ string) (string, error) {
	panic("SaveFile should not be called here")
}

func (f *fakeFileService) SaveBytes(_ context.Context, data []byte, tenantID uint64, fileName string, _ bool) (string, error) {
	if f.saveErr != nil {
		return "", f.saveErr
	}
	if f.saved == nil {
		f.saved = map[string][]byte{}
	}
	f.seq++
	key := "fake://tenant-" + fileName
	f.saved[key] = append([]byte(nil), data...)
	f.tenantIDs = append(f.tenantIDs, tenantID)
	return key, nil
}

func (f *fakeFileService) GetFile(_ context.Context, filePath string) (io.ReadCloser, error) {
	if data, ok := f.saved[filePath]; ok {
		return io.NopCloser(strings.NewReader(string(data))), nil
	}
	return nil, errors.New("file not found")
}

func (f *fakeFileService) GetFileURL(_ context.Context, _ string) (string, error) {
	return "", errors.New("unused")
}

func (f *fakeFileService) DeleteFile(_ context.Context, _ string) error { return nil }

func (f *fakeFileService) CopyFile(_ context.Context, _ string, _ uint64, _ string) (string, error) {
	return "", errors.New("unused")
}
