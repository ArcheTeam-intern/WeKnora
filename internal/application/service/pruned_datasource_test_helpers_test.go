package service

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/textproto"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// bytesToFileHeader builds the multipart file header upload handlers expect.
// It used to live in datasource_service.go; the external-sync feature is
// retired but the file-replace tests still need the helper.
func bytesToFileHeader(data []byte, filename string) (*multipart.FileHeader, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	partHeader.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return nil, fmt.Errorf("create multipart part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("write data to part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}
	reader := multipart.NewReader(&buf, writer.Boundary())
	form, err := reader.ReadForm(int64(len(data)) + 1024)
	if err != nil {
		return nil, fmt.Errorf("read multipart form: %w", err)
	}
	files := form.File["file"]
	if len(files) == 0 {
		return nil, fmt.Errorf("no file in multipart form")
	}
	return files[0], nil
}

// kbDeleteKBRepo records the KB deletion call made by the delete flow tests.
// It used to live in the pruned datasource test file.
type kbDeleteKBRepo struct {
	fakeKBRepo
	deletedID string
}

func (r *kbDeleteKBRepo) DeleteKnowledgeBase(_ context.Context, id string) error {
	r.deletedID = id
	delete(r.rows, id)
	return nil
}

// processSyncTenantRepo pins one tenant for repository lookups. It used to
// live in the pruned datasource test file and is still used by the FAQ
// owner-tenant test.
type processSyncTenantRepo struct {
	interfaces.TenantRepository
	tenant *types.Tenant
}

func (r *processSyncTenantRepo) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return r.tenant, nil
}
