package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

var (
	jwtSecretOnce sync.Once
	jwtSecret     string

	// ErrUserEmailExists is returned by Register when the target entity's
	// email already exists.
	ErrUserEmailExists = errors.New("user with this email already exists")

	// ErrUserUsernameExists is returned by Register when the target entity's
	// username already exists.
	ErrUserUsernameExists = errors.New("user with this username already exists")

	// ErrUserIdentityConflict is returned by AdminCreateUser when only part
	// of the requested identity (email or username) collides with an existing
	// user, so a blind idempotent retry would return the wrong account.
	ErrUserIdentityConflict = errors.New("email and username refer to conflicting existing identities")

	// ErrPasswordPolicy is returned when a newly chosen password does not
	// meet the product's public 8-32 character, letter-and-number contract.
	// It is exported so HTTP handlers can translate the failure to a 400
	// without exposing bcrypt or persistence errors.
	ErrPasswordPolicy = errors.New(
		"password must be 8-32 characters and contain at least one letter and one number")
	// ErrComplexPasswordPolicy is returned when the runtime complex-password
	// switch is on and the new password is missing a required character class.
	ErrComplexPasswordPolicy = errors.New(
		"password must be 8-32 characters and must contain uppercase and lowercase " +
			"letters, numbers, and special characters")

	// ErrInvalidOldPassword is returned by ChangePassword when the supplied
	// current password does not match the stored hash. Handlers map this to
	// a 400 so callers can prompt the user without treating it as a 500.
	ErrInvalidOldPassword = errors.New("invalid old password")

	// ErrSamePassword is returned when the new password equals the current
	// one so callers can reject no-op rotations that would still revoke
	// every session.
	ErrSamePassword = errors.New("new password must differ from current password")
)

// Machine-readable change-password failure reasons for HTTP details fields.
const (
	DetailInvalidOldPassword = "invalid_old_password"
	DetailPasswordPolicy     = "password_policy"
	DetailSamePassword       = "same_password"
)

// getJwtSecret retrieves the JWT secret from the environment, falling back to a securely generated random secret.
func getJwtSecret() string {
	jwtSecretOnce.Do(func() {
		envSecret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
		if envSecret != "" && envSecret != "weknora-jwt-secret" && envSecret != "CHANGE-ME-jwt-secret" {
			jwtSecret = envSecret
			return
		}

		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			panic(fmt.Sprintf("failed to generate JWT secret: %v", err))
		}
		jwtSecret = base64.StdEncoding.EncodeToString(randomBytes)
	})

	return jwtSecret
}

// userService implements the UserService interface
type userService struct {
	userRepo         interfaces.UserRepository
	tokenRepo        interfaces.AuthTokenRepository
	tenantService    interfaces.TenantService
	memberService    interfaces.TenantMemberService
	config           *config.Config
	systemSettingSvc interfaces.SystemSettingService
}

// NewUserService creates a new user service instance
func NewUserService(
	configInfo *config.Config,
	userRepo interfaces.UserRepository,
	tokenRepo interfaces.AuthTokenRepository,
	tenantService interfaces.TenantService,
	memberService interfaces.TenantMemberService,
	systemSettingSvc interfaces.SystemSettingService,
) interfaces.UserService {
	return &userService{
		userRepo:         userRepo,
		tokenRepo:        tokenRepo,
		tenantService:    tenantService,
		memberService:    memberService,
		config:           configInfo,
		systemSettingSvc: systemSettingSvc,
	}
}

func (s *userService) complexPasswordEnabled(ctx context.Context) bool {
	return ResolveComplexPasswordEnabled(ctx, s.config, s.systemSettingSvc)
}

// Register creates a new user account
func (s *userService) Register(ctx context.Context, req *types.RegisterRequest) (*types.User, error) {
	logger.Info(ctx, "Start user registration")

	// Validate input
	if req.Username == "" || req.Email == "" || req.Password == "" {
		return nil, errors.New("username, email and password are required")
	}

	// Check if user already exists
	existingUser, _ := s.userRepo.GetUserByEmail(ctx, req.Email)
	if existingUser != nil {
		return nil, ErrUserEmailExists
	}

	existingUser, _ = s.userRepo.GetUserByUsername(ctx, req.Username)
	if existingUser != nil {
		return nil, ErrUserUsernameExists
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		logger.Errorf(ctx, "Failed to hash password: %v", err)
		return nil, errors.New("failed to process password")
	}

	provisioning := req.TenantProvisioning
	if provisioning == "" {
		provisioning = types.TenantProvisioningCreatePersonal
	}
	if !provisioning.IsValid() {
		return nil, fmt.Errorf("invalid tenant provisioning mode %q", provisioning)
	}

	var createdTenant *types.Tenant
	if provisioning == types.TenantProvisioningCreatePersonal {
		// Note: RetrieverEngines is left empty - system will use defaults
		// from RETRIEVE_DRIVER env.
		tenant := &types.Tenant{
			Name:        fmt.Sprintf("%s's Workspace", secutils.SanitizeForLog(req.Username)),
			Description: "Default workspace",
			Status:      "active",
		}

		createdTenant, err = s.tenantService.CreateTenant(ctx, tenant)
		if err != nil {
			logger.Errorf(ctx, "Failed to create workspace")
			return nil, errors.New("failed to create workspace")
		}
	}

	// Create user
	user := &types.User{
		ID:           uuid.New().String(),
		Username:     req.Username,
		Email:        req.Email,
		PasswordHash: string(hashedPassword),
		TenantID:     0,
		IsActive:     true,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if createdTenant != nil {
		user.TenantID = createdTenant.ID
	}

	err = s.userRepo.CreateUser(ctx, user)
	if err != nil {
		logger.Errorf(ctx, "Failed to create user: %v", err)
		if createdTenant != nil {
			if rollbackErr := s.tenantService.DeleteTenant(ctx, createdTenant.ID); rollbackErr != nil {
				logger.Errorf(ctx, "Failed to roll back tenant %d after user creation failure: %v", createdTenant.ID, rollbackErr)
			}
		}
		return nil, errors.New("failed to create user")
	}

	// Bootstrap an Owner membership so the registrant has full control over
	// the tenant their account just created. Failure here only logs — the
	// user record exists and the auth middleware's orphan-tenant recovery
	// path will recreate the membership on next login.
	if createdTenant != nil && s.memberService != nil {
		if _, err := s.memberService.EnsureOwner(ctx, user.ID, createdTenant.ID); err != nil {
			logger.Errorf(ctx, "Failed to create owner membership for user %s tenant %d: %v",
				user.ID, createdTenant.ID, err)
			_ = s.userRepo.DeleteUser(ctx, user.ID)
			_ = s.tenantService.DeleteTenant(ctx, createdTenant.ID)
			return nil, errors.New("failed to finalise workspace ownership")
		}
	}

	logger.Info(ctx, "User registered successfully")
	return user, nil
}

// Login authenticates a user and returns tokens
func (s *userService) Login(ctx context.Context, req *types.LoginRequest) (*types.LoginResponse, error) {
	logger.Info(ctx, "Start user login")
	// Get user by email
	user, err := s.userRepo.GetUserByEmail(ctx, req.Email)
	if err != nil {
		logger.Errorf(ctx, "Failed to get user by email: %v", err)
		return &types.LoginResponse{
			Success: false,
			Message: "Invalid email or password",
		}, nil
	}
	if user == nil {
		logger.Warn(ctx, "User not found for email")
		return &types.LoginResponse{
			Success: false,
			Message: "Invalid email or password",
		}, nil
	}

	// Check if user is active
	if !user.IsActive {
		logger.Warn(ctx, "User account is disabled")
		return &types.LoginResponse{
			Success: false,
			Message: "Account is disabled",
		}, nil
	}

	// Verify password
	err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password))
	if err != nil {
		logger.Warn(ctx, "Password verification failed")
		return &types.LoginResponse{
			Success: false,
			Message: "Invalid email or password",
		}, nil
	}
	logger.Info(ctx, "Password verification successful")

	// Generate tokens. Resolve the target tenant once so the JWT claim
	// and the tenant we return below agree — otherwise an honoured
	// "last active tenant" preference would mint a token for tenant N
	// but tell the client they're in their home tenant.
	logger.Info(ctx, "Generating tokens")
	resolvedTenantID := s.resolveLoginTenantID(ctx, user)
	accessToken, refreshToken, err := s.generateTokensForTenant(ctx, user, resolvedTenantID)
	if err != nil {
		logger.Errorf(ctx, "Failed to generate tokens: %v", err)
		return &types.LoginResponse{
			Success: false,
			Message: "Login failed",
		}, nil
	}
	logger.Info(ctx, "Tokens generated successfully")

	// Get tenant information. A zero resolved ID is a valid tenantless
	// identity, not a failed tenant lookup.
	var tenant *types.Tenant
	if resolvedTenantID > 0 {
		tenant, err = s.tenantService.GetTenantByID(ctx, resolvedTenantID)
		if err != nil {
			logger.Warn(ctx, "Failed to get tenant info")
		} else {
			logger.Info(ctx, "Tenant information retrieved successfully")
		}
	}

	memberships := s.buildMembershipsForUser(ctx, user, tenant)

	logger.Info(ctx, "User logged in successfully")
	return &types.LoginResponse{
		Success:      true,
		Message:      "Login successful",
		User:         user,
		ActiveTenant: tenant,
		Memberships:  memberships,
		Token:        accessToken,
		RefreshToken: refreshToken,
	}, nil
}

// buildMembershipsForUser returns the user's tenant memberships projected
// into the login-response shape. activeTenant (if non-nil and matching one
// of the rows) is used to reuse its already-fetched name without a second
// DB lookup; other tenants are looked up individually. Errors are logged
// but never propagated — a missing memberships array degrades gracefully
// to length 0 rather than failing the whole login.
//
// When the membership service is unavailable (e.g. in tests that wire only
// part of the dependency graph), this falls back to a single synthesized
// row built from User.TenantID + the active tenant so callers always get
// at least one entry.
func (s *userService) BuildLoginMemberships(
	ctx context.Context,
	user *types.User,
	activeTenant *types.Tenant,
) []types.Membership {
	return s.buildMembershipsForUser(ctx, user, activeTenant)
}

func (s *userService) buildMembershipsForUser(
	ctx context.Context,
	user *types.User,
	activeTenant *types.Tenant,
) []types.Membership {
	if user == nil {
		return []types.Membership{}
	}
	// Only synthesise a membership from User.TenantID when the membership
	// service is entirely unavailable (partial DI graphs / legacy tests).
	// Once ListByUser is reachable, an empty or fully-filtered result is
	// authoritative: inventing a row from a stale users.tenant_id is what
	// kept removed workspaces visible in the space switcher (#2586).
	if s.memberService == nil {
		return synthFallbackMembership(user, activeTenant)
	}
	rows, err := s.memberService.ListByUser(ctx, user.ID)
	if err != nil {
		logger.Warnf(ctx, "Failed to list memberships for user %s: %v", user.ID, err)
		return []types.Membership{}
	}
	if len(rows) == 0 {
		return []types.Membership{}
	}
	// 收集需要批量查询名称的 tenant id（跳过 activeTenant 因为它已经在手）。
	needsLookup := make([]uint64, 0, len(rows))
	for _, m := range rows {
		if m == nil || m.Status != types.TenantMemberStatusActive {
			continue
		}
		if activeTenant != nil && m.TenantID == activeTenant.ID {
			continue
		}
		needsLookup = append(needsLookup, m.TenantID)
	}
	tenantByID := map[uint64]*types.Tenant{}
	if len(needsLookup) > 0 {
		if found, terr := s.tenantService.GetTenantsByIDs(ctx, needsLookup); terr == nil {
			tenantByID = found
		} else {
			logger.Warnf(ctx, "Failed to batch-load tenants for memberships (user=%s): %v",
				user.ID, terr)
		}
	}

	out := make([]types.Membership, 0, len(rows))
	for _, m := range rows {
		if m == nil || m.Status != types.TenantMemberStatusActive {
			continue
		}
		name := ""
		if activeTenant != nil && m.TenantID == activeTenant.ID {
			name = activeTenant.Name
		} else if t, ok := tenantByID[m.TenantID]; ok && t != nil {
			name = t.Name
		}
		// Drop memberships whose tenant row is gone (deleted tenant or
		// stale tenant_members left over from before cascade delete).
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, types.Membership{
			TenantID:   m.TenantID,
			TenantName: name,
			Role:       m.Role,
		})
	}
	return out
}

// synthFallbackMembership returns a single-row membership list inferred
// from User.TenantID. Used only when the membership service itself is
// unavailable (partial DI graphs in tests, or a rollout window where the
// service has not been wired yet) so the response shape stays consistent.
//
// Callers that successfully queried tenant_members must NOT use this
// helper: an empty membership list is authoritative and synthesising
// from users.tenant_id would re-surface workspaces the user was removed
// from (#2586).
//
// The fallback role is intentionally TenantRoleViewer (least privilege):
// the login response only feeds UI rendering, and the backend re-derives
// the real role from tenant_members on every request. If membership data
// is temporarily unavailable, showing a Viewer UI is preferable to
// granting a misleading Owner UI that would surface admin controls the
// backend will then 403. Once the membership row appears (via the auth
// middleware's home-tenant auto-promotion or an admin invitation) the
// next /auth/me-style refresh will upgrade the UI to the real role.
func synthFallbackMembership(user *types.User, activeTenant *types.Tenant) []types.Membership {
	if user == nil || user.TenantID == 0 {
		// Always return a non-nil slice so the login response carries an
		// empty array rather than `null`, preserving the documented
		// "always populated" contract on LoginResponse.Memberships.
		return []types.Membership{}
	}
	name := ""
	if activeTenant != nil && activeTenant.ID == user.TenantID {
		name = activeTenant.Name
	}
	return []types.Membership{{
		TenantID:   user.TenantID,
		TenantName: name,
		Role:       types.TenantRoleViewer,
	}}
}

// GetUserByID gets a user by ID
func (s *userService) GetUserByID(ctx context.Context, id string) (*types.User, error) {
	return s.userRepo.GetUserByID(ctx, id)
}

// GetUsersByIDs proxies to the repository batch fetch. Returns an empty
// map for an empty input; missing ids are absent from the result.
func (s *userService) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*types.User, error) {
	return s.userRepo.GetUsersByIDs(ctx, ids)
}

// GetUserByEmail gets a user by email
func (s *userService) GetUserByEmail(ctx context.Context, email string) (*types.User, error) {
	return s.userRepo.GetUserByEmail(ctx, email)
}

// GetUserByUsername gets a user by username
func (s *userService) GetUserByUsername(ctx context.Context, username string) (*types.User, error) {
	return s.userRepo.GetUserByUsername(ctx, username)
}

// GetUserByTenantID gets the first user (owner) of a tenant
func (s *userService) GetUserByTenantID(ctx context.Context, tenantID uint64) (*types.User, error) {
	return s.userRepo.GetUserByTenantID(ctx, tenantID)
}

// UpdateUser updates user information
func (s *userService) UpdateUser(ctx context.Context, user *types.User) error {
	user.UpdatedAt = time.Now()
	return s.userRepo.UpdateUser(ctx, user)
}

// ListSystemAdmins lists users with IsSystemAdmin=true. Thin pass-through
// to the repository; the handler enforces SystemAdmin gating, so the
// service does not duplicate the role check here.
func (s *userService) ListSystemAdmins(
	ctx context.Context, offset, limit int,
) ([]*types.User, int64, error) {
	return s.userRepo.ListSystemAdmins(ctx, offset, limit)
}

// RevokeSystemAdmin removes system-admin privileges through the
// repository's transactional guard so concurrent revokes cannot remove
// the final administrator.
func (s *userService) RevokeSystemAdmin(ctx context.Context, userID, actorID string) (*types.User, error) {
	return s.userRepo.RevokeSystemAdmin(ctx, userID, actorID)
}

// UpdateUserPreferences applies a partial update over the user's
// preferences blob. PATCH semantics: only keys present in `patch`
// (non-nil pointer fields) replace the existing value; everything else
// is preserved. This lets the front-end PUT only the preference that
// changed without having to read-modify-write the whole struct, and
// also makes the endpoint forward-compatible — older clients that
// don't know about newer keys won't accidentally erase them.
func (s *userService) UpdateUserPreferences(
	ctx context.Context,
	userID string,
	patch types.UserPreferences,
) (types.UserPreferences, error) {
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return types.UserPreferences{}, err
	}

	merged := user.Preferences
	if patch.LastActiveTenantID != nil {
		// *0 = "forget my preference, fall back to home on next login";
		// any positive value = set/replace. We do not validate membership
		// here — invalid values get culled on the next login via
		// resolveLoginTenantID, keeping this endpoint cheap.
		if *patch.LastActiveTenantID == 0 {
			merged.LastActiveTenantID = nil
		} else {
			v := *patch.LastActiveTenantID
			merged.LastActiveTenantID = &v
		}
	}
	if patch.Gallery != nil {
		// Whole-object replace per key: the gallery sends its complete
		// current state (mode + full status map) on every change, so there
		// is no meaningful partial-merge within the block. Values are
		// clamped so a stray client cannot bloat the column or poison the
		// mode.
		g := merged.Gallery
		if g == nil {
			g = &types.GalleryUserPrefs{}
		}
		if patch.Gallery.Mode == types.GalleryModeAll || patch.Gallery.Mode == types.GalleryModeCustom {
			g.Mode = patch.Gallery.Mode
		}
		if patch.Gallery.Status != nil {
			status := make(map[string]string, len(patch.Gallery.Status))
			for id, v := range patch.Gallery.Status {
				if (v == types.GalleryStatusOn || v == types.GalleryStatusOff) &&
					len(id) <= 128 && len(status) < 200 {
					status[id] = v
				}
			}
			g.Status = status
		}
		merged.Gallery = g
	}

	user.Preferences = merged
	user.UpdatedAt = time.Now()
	if err := s.userRepo.UpdateUser(ctx, user); err != nil {
		return types.UserPreferences{}, err
	}
	return merged, nil
}

// DeleteUser deletes a user
func (s *userService) DeleteUser(ctx context.Context, id string) error {
	return s.userRepo.DeleteUser(ctx, id)
}

// ChangePassword changes user password after verifying the current
// credential. The new password must satisfy ValidatePasswordPolicy so
// self-service rotation cannot introduce weaker passwords than
// registration / admin reset allow. On success every outstanding session
// is revoked so a stolen token cannot survive the rotation.
func (s *userService) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}

	// Verify old password before policy checks so callers with a wrong
	// current credential get a clear failure instead of a policy error.
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPassword)); err != nil {
		return ErrInvalidOldPassword
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(newPassword)); err == nil {
		return ErrSamePassword
	}

	if err := ValidatePasswordPolicy(newPassword, s.complexPasswordEnabled(ctx)); err != nil {
		return err
	}

	// Hash new password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	user.PasswordHash = string(hashedPassword)
	user.UpdatedAt = time.Now()
	if user.Preferences.OidcOnlyLogin != nil && *user.Preferences.OidcOnlyLogin {
		cleared := false
		user.Preferences.OidcOnlyLogin = &cleared
	}

	if err := s.userRepo.UpdateUser(ctx, user); err != nil {
		return err
	}

	// Invalidate every outstanding session so a stolen token cannot
	// survive a password rotation.
	return s.tokenRepo.RevokeTokensByUserID(ctx, userID)
}

// AdminResetPassword replaces a user's password without checking the previous
// credential. Authorization and the cannot-reset-self rule live at the system
// admin HTTP boundary; this service owns the security-critical persistence and
// session invalidation so no caller can accidentally update only one of them.
func (s *userService) AdminResetPassword(ctx context.Context, userID, newPassword string) error {
	if err := ValidatePasswordPolicy(newPassword, s.complexPasswordEnabled(ctx)); err != nil {
		return err
	}

	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	user.PasswordHash = string(hashedPassword)
	user.UpdatedAt = time.Now()
	if err := s.userRepo.UpdateUser(ctx, user); err != nil {
		return err
	}

	return s.tokenRepo.RevokeTokensByUserID(ctx, userID)
}

// AdminCreateUser provisions a new local user on behalf of a SystemAdmin.
//
// An absent password generates a random one, returned exactly once.
// Any provided password, must satisfy ValidatePasswordPolicy.
//
// Delegates to Register, so duplicate checks, tenant provisioning and Owner
// membership bootstrapping match public registration.
func (s *userService) AdminCreateUser(
	ctx context.Context,
	req *types.AdminCreateUserRequest,
	provisioning types.TenantProvisioningMode,
) (*types.User, string, error) {
	if req == nil || strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Email) == "" {
		return nil, "", errors.New("username and email are required")
	}

	password := ""
	generated := false
	complexPasswordEnabled := s.complexPasswordEnabled(ctx)

	if req.Password == nil {
		randomPassword, err := generatePolicyCompliantPassword(complexPasswordEnabled)
		if err != nil {
			return nil, "", fmt.Errorf("failed to generate password: %w", err)
		}
		password = randomPassword
		generated = true
	} else {
		password = *req.Password
	}
	// Generation triggers only on an absent password. Any provided
	// value, empty or whitespace-only, is hashed byte-for-byte and must
	// satisfy the password policy.
	if err := ValidatePasswordPolicy(password, complexPasswordEnabled); err != nil {
		return nil, "", err
	}

	user, err := s.Register(ctx, &types.RegisterRequest{
		Username:           strings.TrimSpace(req.Username),
		Email:              strings.TrimSpace(req.Email),
		Password:           password,
		TenantProvisioning: provisioning,
	})
	// WARN: idempotency is sequential only. Two concurrent creates of the
	// same identity can race past Register's check; the loser gets a 500,
	// and a retry resolves idempotently.
	if err != nil {
		// Register owns duplicate detection; on a duplicate we surface
		// the existing row so the caller can respond idempotently. The
		// sentinel names the colliding identity, so the lookup is
		// targeted at exactly that key.
		switch {
		case errors.Is(err, ErrUserEmailExists):
			return s.adminCreateUserOnDuplicate(
				ctx, req, err,
				func(ctx context.Context) (*types.User, error) {
					return s.userRepo.GetUserByEmail(ctx, strings.TrimSpace(req.Email))
				},
			)
		case errors.Is(err, ErrUserUsernameExists):
			return s.adminCreateUserOnDuplicate(
				ctx, req, err,
				func(ctx context.Context) (*types.User, error) {
					return s.userRepo.GetUserByUsername(ctx, strings.TrimSpace(req.Username))
				},
			)
		}
		return nil, "", err
	}
	if generated {
		return user, password, nil
	}
	return user, "", nil
}

func (s *userService) adminCreateUserOnDuplicate(
	ctx context.Context,
	req *types.AdminCreateUserRequest,
	dupErr error,
	lookup func(context.Context) (*types.User, error),
) (*types.User, string, error) {
	existing, lookupErr := lookup(ctx)
	if lookupErr != nil || existing == nil {
		return nil, "", dupErr
	}
	if adminCreateIdentityMatches(existing, req.Username, req.Email) {
		return existing, "", dupErr
	}
	return nil, "", ErrUserIdentityConflict
}

// adminCreateIdentityMatches reports whether an existing row is the exact
// identity an admin create is idempotently retrying (both email and username).
func adminCreateIdentityMatches(existing *types.User, username, email string) bool {
	if existing == nil {
		return false
	}
	return existing.Username == strings.TrimSpace(username) &&
		existing.Email == strings.TrimSpace(email)
}

// ValidatePassword validates user password
func (s *userService) ValidatePassword(ctx context.Context, userID string, password string) error {
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}

	return bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
}

// GenerateTokens generates access and refresh tokens for user. The
// access token's tenant_id claim defaults to user.TenantID (home), but
// if the user has persisted a still-valid "last active tenant"
// preference we honour it instead — so login (and the refresh-token
// rotation path that also calls into here) lands the user back where
// they left off across devices. SwitchTenant remains the explicit tool
// for switching to an arbitrary membership.
func (s *userService) GenerateTokens(
	ctx context.Context,
	user *types.User,
) (accessToken, refreshToken string, err error) {
	return s.generateTokensForTenant(ctx, user, s.resolveLoginTenantID(ctx, user))
}

// resolveLoginTenantID picks the tenant whose ID should be encoded in a
// freshly minted access token. The contract:
//
//  1. If the user has no LastActiveTenantID preference set (or it points
//     at home), return home — the historical behaviour. A tenantless user
//     with an active membership adopts their earliest membership instead;
//     this repairs partial invitation/admin-assignment flows.
//  2. Otherwise validate the preference: the tenant must still exist and
//     the user must still have an active membership (or be a cross-tenant
//     superuser). Validation failure logs a warning, best-effort clears
//     the stale preference (so we don't waste a DB round-trip on every
//     subsequent login), and falls back to home.
//
// This is intentionally a private method on userService so it can reach
// memberService / tenantService / userRepo. Errors from the validation
// path never fail login; the worst case is the user lands in home.
func (s *userService) resolveLoginTenantID(ctx context.Context, user *types.User) uint64 {
	if user == nil {
		return 0
	}
	pref := user.Preferences.LastActiveTenantID
	if pref == nil || *pref == 0 || *pref == user.TenantID {
		return s.homeOrFirstMembershipTenant(ctx, user)
	}
	preferred := *pref

	// Tenant must still exist.
	if s.tenantService != nil {
		if _, err := s.tenantService.GetTenantByID(ctx, preferred); err != nil {
			logger.Warnf(ctx,
				"resolveLoginTenantID: preferred tenant %d not loadable for user %s, "+
					"clearing preference and falling back to home: %v",
				preferred, user.ID, err)
			s.clearLastActiveTenantPreference(ctx, user)
			return s.homeOrFirstMembershipTenant(ctx, user)
		}
	}

	// Membership (or cross-tenant superuser) must still be valid. Mirrors
	// the gate in SwitchTenant so the two entry points stay consistent.
	if !user.CanAccessAllTenants {
		if s.memberService == nil {
			logger.Warnf(ctx,
				"resolveLoginTenantID: member service unavailable; falling back to home for user %s",
				user.ID)
			return user.TenantID
		}
		member, err := s.memberService.GetMembership(ctx, user.ID, preferred)
		if err != nil || member == nil || member.Status != types.TenantMemberStatusActive {
			logger.Warnf(ctx,
				"resolveLoginTenantID: user %s no longer has active membership in tenant %d, "+
					"clearing preference and falling back to home (err=%v)",
				user.ID, preferred, err)
			s.clearLastActiveTenantPreference(ctx, user)
			return s.homeOrFirstMembershipTenant(ctx, user)
		}
	}

	return preferred
}

// homeOrFirstMembershipTenant returns the user's home tenant, or — for a
// tenantless identity (TenantID == 0) — the earliest active membership.
// Shared by the happy path and the stale-preference fallbacks so a
// tenantless session with a valid membership never gets a zero-tenant
// token when a usable tenant is available (repairs partial
// invitation/admin-assignment flows). resolveFirstMembershipTenant
// best-effort persists the resolved tenant as the new home.
//
// When users.tenant_id is non-zero we still verify an active membership
// still exists (mirroring resolveLoginTenantID's check on
// LastActiveTenantID). A dangling home pointer is common after
// RemoveMember: the membership row is soft-deleted but users.tenant_id
// was historically left untouched, which made the removed workspace
// reappear via synthFallbackMembership (#2586). Superusers that can
// access every tenant skip the membership gate.
func (s *userService) homeOrFirstMembershipTenant(ctx context.Context, user *types.User) uint64 {
	if user == nil {
		return 0
	}
	if user.TenantID == 0 {
		return s.resolveFirstMembershipTenant(ctx, user)
	}
	if user.CanAccessAllTenants || s.memberService == nil {
		return user.TenantID
	}
	member, err := s.memberService.GetMembership(ctx, user.ID, user.TenantID)
	if err == nil && member != nil && member.Status == types.TenantMemberStatusActive {
		return user.TenantID
	}
	logger.Warnf(ctx,
		"homeOrFirstMembershipTenant: user %s home tenant %d has no active membership, "+
			"clearing stale home and re-resolving (err=%v)",
		user.ID, user.TenantID, err)
	s.clearStaleHomeTenant(ctx, user)
	return s.resolveFirstMembershipTenant(ctx, user)
}

// clearStaleHomeTenant best-effort zeroes users.tenant_id (and a
// LastActiveTenantID that pointed at the same workspace) after the home
// membership is observed to be gone. Failures are logged but never
// fail login: the in-memory user is already corrected for this request.
func (s *userService) clearStaleHomeTenant(ctx context.Context, user *types.User) {
	if user == nil {
		return
	}
	staleHome := user.TenantID
	user.TenantID = 0
	if user.Preferences.LastActiveTenantID != nil && *user.Preferences.LastActiveTenantID == staleHome {
		user.Preferences.LastActiveTenantID = nil
	}
	if s.userRepo == nil {
		return
	}
	if err := s.userRepo.UpdateUser(ctx, user); err != nil {
		logger.Warnf(ctx,
			"clearStaleHomeTenant: failed to persist cleared home for user %s (was tenant %d): %v",
			user.ID, staleHome, err)
	}
}

// resolveFirstMembershipTenant makes a tenantless identity usable when an
// active membership already exists (for example, an invitation was accepted
// but persisting the default tenant failed). ListByUser is stably ordered by
// join time, so the earliest valid membership is deterministic. Persisting it
// as home is best-effort: even if the repair write fails, the freshly issued
// token can still be scoped to the membership and the next login retries.
func (s *userService) resolveFirstMembershipTenant(ctx context.Context, user *types.User) uint64 {
	if user == nil || s.memberService == nil {
		return 0
	}
	members, err := s.memberService.ListByUser(ctx, user.ID)
	if err != nil {
		logger.Warnf(ctx, "resolveLoginTenantID: failed to list memberships for tenantless user %s: %v", user.ID, err)
		return 0
	}
	for _, member := range members {
		if member == nil || member.TenantID == 0 || member.Status != types.TenantMemberStatusActive {
			continue
		}
		if s.tenantService != nil {
			if _, err := s.tenantService.GetTenantByID(ctx, member.TenantID); err != nil {
				logger.Warnf(ctx, "resolveLoginTenantID: tenant %d for tenantless user %s is unavailable: %v",
					member.TenantID, user.ID, err)
				continue
			}
		}

		user.TenantID = member.TenantID
		if s.userRepo != nil {
			if err := s.userRepo.UpdateUser(ctx, user); err != nil {
				logger.Warnf(ctx, "resolveLoginTenantID: failed to persist tenant %d for tenantless user %s: %v",
					member.TenantID, user.ID, err)
				user.TenantID = 0
			}
		}
		return member.TenantID
	}
	return 0
}

// clearLastActiveTenantPreference is the best-effort cleanup half of
// resolveLoginTenantID. Failures here are logged but never propagated:
// the in-memory user already has the preference cleared for this login,
// and the next login will re-attempt the cleanup.
func (s *userService) clearLastActiveTenantPreference(ctx context.Context, user *types.User) {
	if user == nil {
		return
	}
	user.Preferences.LastActiveTenantID = nil
	if err := s.userRepo.UpdateUser(ctx, user); err != nil {
		logger.Warnf(ctx,
			"clearLastActiveTenantPreference: failed to persist cleared preference for user %s: %v",
			user.ID, err)
	}
}

// generateTokensForTenant is the shared implementation behind
// GenerateTokens and SwitchTenant. It encodes activeTenantID into the
// access token's tenant_id claim so the auth middleware scopes future
// requests there.
func (s *userService) generateTokensForTenant(
	ctx context.Context,
	user *types.User,
	activeTenantID uint64,
) (accessToken, refreshToken string, err error) {
	// Generate access token (expires in 24 hours)
	accessClaims := jwt.MapClaims{
		"user_id":   user.ID,
		"email":     user.Email,
		"tenant_id": activeTenantID,
		"exp":       time.Now().Add(24 * time.Hour).Unix(),
		"iat":       time.Now().Unix(),
		"type":      "access",
	}

	accessTokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessToken, err = accessTokenObj.SignedString([]byte(getJwtSecret()))
	if err != nil {
		return "", "", err
	}

	// Generate refresh token (expires in 7 days)
	refreshClaims := jwt.MapClaims{
		"user_id": user.ID,
		"exp":     time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat":     time.Now().Unix(),
		"type":    "refresh",
	}

	refreshTokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshToken, err = refreshTokenObj.SignedString([]byte(getJwtSecret()))
	if err != nil {
		return "", "", err
	}

	// Store tokens in database
	accessTokenRecord := &types.AuthToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     accessToken,
		TokenType: "access_token",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	refreshTokenRecord := &types.AuthToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     refreshToken,
		TokenType: "refresh_token",
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Persist before returning the pair: a signed token the database has no row
	// for is rejected by ValidateToken / RefreshToken on the very next request,
	// so swallowing a write error turns a transient database failure into a
	// login that looks successful and then loses the session immediately.
	if err := s.tokenRepo.CreateToken(ctx, accessTokenRecord); err != nil {
		return "", "", fmt.Errorf("persist access token: %w", err)
	}
	if err := s.tokenRepo.CreateToken(ctx, refreshTokenRecord); err != nil {
		return "", "", fmt.Errorf("persist refresh token: %w", err)
	}

	return accessToken, refreshToken, nil
}

// SwitchTenant verifies that user has an active membership in
// targetTenantID and issues a new token pair scoped to that tenant.
// The previous refresh token (if provided) is revoked so the old session
// can no longer roll forward into the source tenant.
//
// On success the target is also recorded as the user's last-active-
// tenant preference. Refresh tokens do not carry tenant_id, so
// RefreshToken / the next login re-resolve from this preference;
// a successful switch therefore has to persist it before minting
// tokens. A write failure aborts the switch so we never return a
// token pair whose later refresh would bounce to a different workspace.
//
// Returns ErrMembershipNotFound when the user is not a member of the
// target tenant. Cross-tenant superuser access (CanAccessAllTenants)
// is allowed without a membership row, mirroring the auth middleware's
// resolveTenantRole behaviour.
func (s *userService) SwitchTenant(
	ctx context.Context,
	user *types.User,
	targetTenantID uint64,
	currentRefreshToken string,
) (*types.LoginResponse, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	if targetTenantID == 0 {
		return nil, errors.New("target workspace ID is required")
	}

	// Verify membership unless the caller is a cross-tenant superuser
	// switching outside their home tenant.
	if !user.CanAccessAllTenants || targetTenantID == user.TenantID {
		if s.memberService == nil {
			return nil, errors.New("workspace membership service unavailable")
		}
		member, err := s.memberService.GetMembership(ctx, user.ID, targetTenantID)
		if err != nil {
			return nil, fmt.Errorf("lookup membership: %w", err)
		}
		if member == nil || member.Status != types.TenantMemberStatusActive {
			return nil, ErrMembershipNotFound
		}
	}

	tenant, err := s.tenantService.GetTenantByID(ctx, targetTenantID)
	if err != nil {
		return nil, fmt.Errorf("load target workspace: %w", err)
	}

	// Persist before minting tokens so a 200 response is also a
	// durable landing-preference update. RefreshToken re-reads this
	// field; writing after issue would leave a window where access
	// is scoped to targetTenantID but refresh falls back to home.
	if err := s.recordLastActiveTenant(ctx, user, targetTenantID); err != nil {
		return nil, fmt.Errorf("record last-active-tenant preference: %w", err)
	}

	accessToken, refreshToken, err := s.generateTokensForTenant(ctx, user, targetTenantID)
	if err != nil {
		return nil, fmt.Errorf("generate tokens: %w", err)
	}

	// Best-effort revoke of the previous refresh token. Failure is
	// logged but not fatal — the new tokens are already issued and the
	// old refresh token will expire naturally.
	if strings.TrimSpace(currentRefreshToken) != "" {
		if err := s.RevokeToken(ctx, currentRefreshToken); err != nil {
			logger.Warnf(ctx, "Failed to revoke previous refresh token during tenant switch: %v", err)
		}
	}

	memberships := s.buildMembershipsForUser(ctx, user, tenant)

	return &types.LoginResponse{
		Success:      true,
		Message:      "Workspace switched",
		User:         user,
		ActiveTenant: tenant,
		Memberships:  memberships,
		Token:        accessToken,
		RefreshToken: refreshToken,
	}, nil
}

// recordLastActiveTenant records tenantID as the user's last-active-
// tenant preference so fresh logins and refresh-token rotations land
// in the last activated workspace. Switching home writes the home ID
// rather than clearing; resolveLoginTenantID treats *home the same as
// nil today. (The SPA still sends 0 when switching home — landing is
// equivalent unless home tenant_id is later rewritten while the old
// home membership remains.) Errors are returned to the caller.
func (s *userService) recordLastActiveTenant(ctx context.Context, user *types.User, tenantID uint64) error {
	if user == nil || tenantID == 0 {
		return nil
	}
	patch := types.UserPreferences{LastActiveTenantID: &tenantID}
	merged, err := s.UpdateUserPreferences(ctx, user.ID, patch)
	if err != nil {
		return err
	}
	// Reflect the persisted preferences in the switch response.
	user.Preferences = merged
	return nil
}

// ValidateToken validates an access token. The second return value is
// the JWT's `tenant_id` claim — i.e. the tenant the token was minted
// for, which may differ from user.TenantID after a /auth/switch-tenant
// call. Tokens minted before tenant-level RBAC don't carry the claim;
// in that case we fall back to user.TenantID for backward compatibility.
func (s *userService) ValidateToken(ctx context.Context, tokenString string) (*types.User, uint64, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(getJwtSecret()), nil
	})

	if err != nil || !token.Valid {
		return nil, 0, errors.New("invalid token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, 0, errors.New("invalid token claims")
	}

	userID, ok := claims["user_id"].(string)
	if !ok {
		return nil, 0, errors.New("invalid user ID in token")
	}

	if isRefreshTokenClaims(claims) {
		return nil, 0, errors.New("refresh token cannot be used as access token")
	}

	// Check if token is revoked. A failed lookup is an infrastructure error and
	// is reported as such: collapsing it into "token is revoked" claims a
	// revocation that never happened and sends operators down the wrong path.
	tokenRecord, err := s.tokenRepo.GetTokenByValue(ctx, tokenString)
	if err != nil {
		return nil, 0, fmt.Errorf("look up token: %w", err)
	}
	if tokenRecord == nil || tokenRecord.IsRevoked {
		return nil, 0, errors.New("token is revoked")
	}
	if tokenRecord.TokenType == "refresh_token" {
		return nil, 0, errors.New("refresh token cannot be used as access token")
	}

	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, 0, err
	}

	// Extract active tenant from the JWT. Anything missing or unparseable
	// falls back to the user's home tenant so old tokens (and tokens issued
	// by code paths that don't yet set the claim) keep working.
	activeTenantID := tenantIDFromClaims(claims, user.TenantID)

	return user, activeTenantID, nil
}

func redactAuthToken(token *types.AuthToken) *types.AuthToken {
	if token == nil {
		return nil
	}
	cp := *token
	cp.Token = ""
	return &cp
}

// GetAccessTokenByValue looks up the stored token row for a JWT string.
// The JWT itself is stripped so callers cannot log or re-play it.
func (s *userService) GetAccessTokenByValue(ctx context.Context, tokenString string) (*types.AuthToken, error) {
	tokenString = strings.TrimSpace(tokenString)
	if tokenString == "" {
		return nil, apprepo.ErrTokenNotFound
	}
	token, err := s.tokenRepo.GetTokenByValue(ctx, tokenString)
	if err != nil {
		return nil, err
	}
	return redactAuthToken(token), nil
}

// GetAccessTokenByID looks up a stored token row by primary key.
func (s *userService) GetAccessTokenByID(ctx context.Context, id string) (*types.AuthToken, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, apprepo.ErrTokenNotFound
	}
	token, err := s.tokenRepo.GetTokenByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return redactAuthToken(token), nil
}

func isRefreshTokenClaims(claims jwt.MapClaims) bool {
	tokenType, ok := claims["type"].(string)
	return ok && tokenType == "refresh"
}

func userIDFromSignedToken(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(getJwtSecret()), nil
	}, jwt.WithoutClaimsValidation())
	if err != nil || token == nil || !token.Valid {
		return "", errors.New("invalid token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("invalid token claims")
	}

	userID, ok := claims["user_id"].(string)
	if !ok || strings.TrimSpace(userID) == "" {
		return "", errors.New("invalid user ID in token")
	}
	return userID, nil
}

// tenantIDFromClaims pulls the active tenant ID out of a parsed JWT
// claim map. Returns fallback when the claim is missing or has an
// unrecognised type. Extracted as a free function so it can be unit
// tested without standing up the full userService dependency graph.
//
// JSON numbers come back as float64 from jwt.MapClaims; the int64 /
// uint64 branches cover legacy code paths and tests that build claims
// directly. Negative values are treated as missing.
func tenantIDFromClaims(claims jwt.MapClaims, fallback uint64) uint64 {
	raw, ok := claims["tenant_id"]
	if !ok {
		return fallback
	}
	switch v := raw.(type) {
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case uint64:
		if v > 0 {
			return v
		}
	}
	return fallback
}

// RefreshToken refreshes access token using refresh token
func (s *userService) RefreshToken(
	ctx context.Context,
	refreshTokenString string,
) (accessToken, newRefreshToken string, err error) {
	token, err := jwt.Parse(refreshTokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(getJwtSecret()), nil
	})

	if err != nil || !token.Valid {
		return "", "", errors.New("invalid refresh token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", errors.New("invalid token claims")
	}

	tokenType, ok := claims["type"].(string)
	if !ok || tokenType != "refresh" {
		return "", "", errors.New("not a refresh token")
	}

	userID, ok := claims["user_id"].(string)
	if !ok {
		return "", "", errors.New("invalid user ID in token")
	}

	// Check if token is revoked. See ValidateToken: a lookup failure must not be
	// reported as a revocation.
	tokenRecord, err := s.tokenRepo.GetTokenByValue(ctx, refreshTokenString)
	if err != nil {
		return "", "", fmt.Errorf("look up refresh token: %w", err)
	}
	if tokenRecord == nil || tokenRecord.IsRevoked {
		return "", "", errors.New("refresh token is revoked")
	}
	if tokenRecord.TokenType != "refresh_token" {
		return "", "", errors.New("not a refresh token")
	}

	// Get user
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return "", "", err
	}

	// Revoke old refresh token
	tokenRecord.IsRevoked = true
	_ = s.tokenRepo.UpdateToken(ctx, tokenRecord)

	// Generate new tokens
	return s.GenerateTokens(ctx, user)
}

// Logout invalidates every outstanding session for the user identified by
// the presented JWT. Access and refresh tokens are both accepted so clients
// can end the session without refreshing first; expired tokens are allowed
// so logout still works after the access token TTL.
func (s *userService) Logout(ctx context.Context, tokenString string) error {
	userID, err := userIDFromSignedToken(tokenString)
	if err != nil {
		return err
	}
	return s.tokenRepo.RevokeTokensByUserID(ctx, userID)
}

// RevokeToken revokes a token
func (s *userService) RevokeToken(ctx context.Context, tokenString string) error {
	tokenRecord, err := s.tokenRepo.GetTokenByValue(ctx, tokenString)
	if err != nil {
		return err
	}

	tokenRecord.IsRevoked = true
	tokenRecord.UpdatedAt = time.Now()

	return s.tokenRepo.UpdateToken(ctx, tokenRecord)
}

// GetCurrentUser gets current user from context
func (s *userService) GetCurrentUser(ctx context.Context) (*types.User, error) {
	user, ok := ctx.Value(types.UserContextKey).(*types.User)
	if !ok {
		return nil, errors.New("user not found in context")
	}

	return user, nil
}

// SearchUsers searches users by username or email
func (s *userService) SearchUsers(ctx context.Context, query string, limit int) ([]*types.User, error) {
	if query == "" {
		return []*types.User{}, nil
	}
	return s.userRepo.SearchUsers(ctx, query, limit)
}

func generateRandomString(length int) (string, error) {
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func getRandomChar(charset string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
	if err != nil {
		return 0, err
	}
	return charset[n.Int64()], nil
}

func generateComplexPassword(length int) (string, error) {
	if length < 4 {
		return "", fmt.Errorf("password length must be at least 4")
	}

	password := make([]byte, 0, length)

	c, err := getRandomChar(upperChars)
	if err != nil {
		return "", err
	}
	password = append(password, c)

	c, err = getRandomChar(lowerChars)
	if err != nil {
		return "", err
	}
	password = append(password, c)

	c, err = getRandomChar(digitChars)
	if err != nil {
		return "", err
	}
	password = append(password, c)

	c, err = getRandomChar(passwordSpecialChars)
	if err != nil {
		return "", err
	}
	password = append(password, c)

	for len(password) < length {
		c, err = getRandomChar(allChars)
		if err != nil {
			return "", err
		}
		password = append(password, c)
	}

	// Fisher-Yates shuffle
	for i := len(password) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}

		j := int(n.Int64())
		password[i], password[j] = password[j], password[i]
	}

	return string(password), nil
}

// generatePolicyCompliantPassword returns a cryptographically random
// password that satisfies ValidatePasswordPolicy.
//
// Simple policies may require regeneration (a single 32-char base64url
// draw misses digits ~0.4% of the time). Complex policies are satisfied
// in a single pass.
func generatePolicyCompliantPassword(complexPasswordEnabled bool) (string, error) {
	var password string
	var err error
	for {
		if complexPasswordEnabled {
			password, err = generateComplexPassword(16)
		} else {
			password, err = generateRandomString(24)
		}

		if err != nil {
			return "", err
		}

		if ValidatePasswordPolicy(password, complexPasswordEnabled) == nil {
			return password, nil
		}
	}
}
