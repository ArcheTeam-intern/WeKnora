package middleware

// fakeTenantService is the shared TenantService test double. It used to live in
// the embed-auth test file; embed auth was removed from the build, but three
// other middleware tests still need the double, so it moved here.

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

type fakeTenantService struct {
	tenant *types.Tenant
}

func (f *fakeTenantService) GetTenantByID(ctx context.Context, id uint64) (*types.Tenant, error) {
	return f.tenant, nil
}

func (f *fakeTenantService) CreateTenant(ctx context.Context, tenant *types.Tenant) (*types.Tenant, error) {
	return nil, nil
}

func (f *fakeTenantService) GetTenantsByIDs(ctx context.Context, ids []uint64) (map[uint64]*types.Tenant, error) {
	return nil, nil
}

func (f *fakeTenantService) UpdateTenant(ctx context.Context, tenant *types.Tenant) (*types.Tenant, error) {
	return nil, nil
}

func (f *fakeTenantService) DeleteTenant(ctx context.Context, id uint64) error {
	return nil
}

func (f *fakeTenantService) ListTenants(ctx context.Context) ([]*types.Tenant, error) {
	return nil, nil
}

func (f *fakeTenantService) ListAllTenants(ctx context.Context) ([]*types.Tenant, error) {
	return nil, nil
}

func (f *fakeTenantService) BulkSetStorageQuota(ctx context.Context, quotaBytes int64) (int64, error) {
	return 0, nil
}

func (f *fakeTenantService) SearchTenants(
	ctx context.Context, keyword string, tenantID uint64, page, pageSize int,
) ([]*types.Tenant, int64, error) {
	return nil, 0, nil
}

func (f *fakeTenantService) GetTenantByIDForUser(
	ctx context.Context, tenantID uint64, userID string,
) (*types.Tenant, error) {
	return f.tenant, nil
}

func (f *fakeTenantService) GetWeKnoraCloudCredentials(ctx context.Context) *types.WeKnoraCloudCredentials {
	return nil
}
