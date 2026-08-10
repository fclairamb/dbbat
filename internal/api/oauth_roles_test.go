package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/auth/oidc"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestConfigKnownRolesMatchStore pins the duplication config has to carry:
// store imports config, so config cannot name store's role constants, and a
// role added on one side without the other would make DBB_OIDC_ROLE_MAPPING
// silently reject (or silently accept) a role.
func TestConfigKnownRolesMatchStore(t *testing.T) {
	t.Parallel()

	assert.ElementsMatch(t,
		[]string{store.RoleAdmin, store.RoleViewer, store.RoleConnector},
		config.KnownRoles,
		"config.KnownRoles must list exactly the roles store defines")
}

// TestResolveMappedRoles is the decision table of the whole feature: what a
// login does to a user's roles given their directory groups. It is a pure
// function precisely so these cases can be stated without a database.
func TestResolveMappedRoles(t *testing.T) {
	t.Parallel()

	mapping := map[string][]string{
		"admin":  {"db-admins", "sre"},
		"viewer": {"analysts"},
	}

	cases := []struct {
		name    string
		current []string
		groups  []string
		mapping map[string][]string
		// defaultRole is the floor; empty means store.RoleConnector, which is
		// what a stock deployment runs.
		defaultRole string
		wantRoles   []string
		wantGranted []string
		wantRevoked []string
	}{
		{
			name:        "promotion: joining the group grants the role",
			current:     []string{"connector"},
			groups:      []string{"db-admins"},
			mapping:     mapping,
			wantRoles:   []string{"connector", "admin"},
			wantGranted: []string{"admin"},
			wantRevoked: []string{},
		},
		{
			name:        "demotion: leaving the group revokes the role",
			current:     []string{"connector", "admin"},
			groups:      []string{"engineering"},
			mapping:     mapping,
			wantRoles:   []string{"connector"},
			wantGranted: []string{},
			wantRevoked: []string{"admin"},
		},
		{
			name:        "any one of a role's groups is enough",
			current:     []string{"connector"},
			groups:      []string{"sre"},
			mapping:     mapping,
			wantRoles:   []string{"connector", "admin"},
			wantGranted: []string{"admin"},
			wantRevoked: []string{},
		},
		{
			name:        "steady state: a matching membership changes nothing",
			current:     []string{"connector", "admin"},
			groups:      []string{"db-admins"},
			mapping:     mapping,
			wantRoles:   []string{"connector", "admin"},
			wantGranted: []string{},
			wantRevoked: []string{},
		},
		{
			name:    "a role the mapping never names survives a login",
			current: []string{"viewer"},
			groups:  []string{"db-admins"},
			// viewer is not mapped here, so nothing in the directory has an
			// opinion about it: an admin granted it by hand and it stays.
			mapping:     map[string][]string{"admin": {"db-admins"}},
			wantRoles:   []string{"viewer", "admin"},
			wantGranted: []string{"admin"},
			wantRevoked: []string{},
		},
		{
			name:        "the default role is the floor, never an empty set",
			current:     []string{"admin"},
			groups:      nil,
			mapping:     map[string][]string{"admin": {"db-admins"}},
			wantRoles:   []string{"connector"},
			wantGranted: []string{"connector"},
			wantRevoked: []string{"admin"},
		},
		{
			name:    "the floor restoring a role the user already held is not a grant",
			current: []string{"viewer"},
			groups:  nil,
			// viewer is both the default role and a mapped one: an unmatched
			// user has it dropped and then handed straight back by the floor.
			// That round trip must read as "nothing happened", or every login
			// would write identical roles and claim a promotion.
			mapping:     map[string][]string{"admin": {"db-admins"}, "viewer": {"analysts"}},
			defaultRole: store.RoleViewer,
			wantRoles:   []string{"viewer"},
			wantGranted: []string{},
			wantRevoked: []string{},
		},
		{
			name:    "the floor still reports a real grant",
			current: []string{"admin"},
			groups:  nil,
			// Here the user genuinely did not hold the default role before.
			mapping:     map[string][]string{"admin": {"db-admins"}, "connector": {"engineering"}},
			wantRoles:   []string{"connector"},
			wantGranted: []string{"connector"},
			wantRevoked: []string{"admin"},
		},
		{
			name:        "group matching is exact, case included",
			current:     []string{"connector"},
			groups:      []string{"DB-Admins"},
			mapping:     map[string][]string{"admin": {"db-admins"}},
			wantRoles:   []string{"connector"},
			wantGranted: []string{},
			wantRevoked: []string{},
		},
		{
			name:        "an unrelated membership grants nothing",
			current:     []string{"connector"},
			groups:      []string{"marketing", "all-staff"},
			mapping:     mapping,
			wantRoles:   []string{"connector"},
			wantGranted: []string{},
			wantRevoked: []string{},
		},
		{
			name:        "several roles move at once",
			current:     []string{"admin"},
			groups:      []string{"analysts"},
			mapping:     mapping,
			wantRoles:   []string{"viewer"},
			wantGranted: []string{"viewer"},
			wantRevoked: []string{"admin"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			defaultRole := tc.defaultRole
			if defaultRole == "" {
				defaultRole = store.RoleConnector
			}

			got := resolveMappedRoles(tc.current, tc.groups, tc.mapping, defaultRole)

			assert.Equal(t, tc.wantRoles, got.Roles)
			assert.Equal(t, tc.wantGranted, got.Granted)
			assert.Equal(t, tc.wantRevoked, got.Revoked)
		})
	}
}

// TestResolveMappedRolesForcedRole covers the mechanism the last-admin guard
// runs on: re-resolving with a role pinned must not leave the floor's default
// role behind as a phantom grant.
func TestResolveMappedRolesForcedRole(t *testing.T) {
	t.Parallel()

	mapping := map[string][]string{"admin": {"db-admins"}}

	got := resolveMappedRoles([]string{"admin"}, nil, mapping, store.RoleConnector, store.RoleAdmin)

	assert.Equal(t, []string{"admin"}, got.Roles, "the pinned role stays, and the floor does not fire")
	assert.False(t, got.changed(), "pinning the only role must be a no-op, not a write")

	// Pinning a role the user does not hold grants nothing: the guard only
	// ever protects what is already there.
	got = resolveMappedRoles([]string{"connector"}, nil, mapping, store.RoleConnector, store.RoleAdmin)
	assert.Equal(t, []string{"connector"}, got.Roles)
}

// oidcOAuthUser builds an OIDC identity carrying the given groups.
func oidcOAuthUser(suffix string, groups ...string) *auth.OAuthUser {
	return &auth.OAuthUser{
		ProviderID:  "OIDC-" + suffix,
		Email:       "oidc-" + suffix + "@example.com",
		DisplayName: "OIDC User " + suffix,
		Groups:      groups,
	}
}

// serverWithRoleMapping points a test server at a role mapping, as if
// DBB_OIDC_ROLE_MAPPING had been set.
func serverWithRoleMapping(t *testing.T, server *Server, mapping string) {
	t.Helper()

	serverWithRoleMappingAndDefault(t, server, mapping, store.RoleConnector)
}

// serverWithRoleMappingAndDefault is serverWithRoleMapping for the deployments
// that moved DBB_SLACK_AUTH_DEFAULT_ROLE off `connector`.
func serverWithRoleMappingAndDefault(t *testing.T, server *Server, mapping, defaultRole string) {
	t.Helper()

	server.config = &config.Config{
		SlackAuth: config.SlackAuthConfig{
			AutoCreateUsers: true,
			DefaultRole:     defaultRole,
		},
		OIDCAuth: config.OIDCAuthConfig{
			Issuer:      "https://issuer.example.test",
			RoleMapping: mapping,
		},
	}
}

// auditEventsFor returns a user's audit entries of one type.
func auditEventsFor(t *testing.T, dataStore *store.Store, eventType string, userID uuid.UUID) []store.AuditEvent {
	t.Helper()

	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{
		EventType: &eventType,
		UserID:    &userID,
		Limit:     10,
	})
	require.NoError(t, err)

	return events
}

// roleSyncEvents returns the audit entries written for a user's role syncs.
func roleSyncEvents(t *testing.T, dataStore *store.Store, userID uuid.UUID) []store.AuditEvent {
	t.Helper()

	return auditEventsFor(t, dataStore, AuditEventOAuthRolesSynced, userID)
}

// TestOAuthRoleSyncPromotesOnEveryLogin covers the promotion half: an existing
// account that joined the directory group is promoted at its next sign-in,
// without an admin touching the user list.
func TestOAuthRoleSyncPromotesOnEveryLogin(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins")

	provider := &mockProvider{name: oidc.ProviderName}
	oauthUser := oidcOAuthUser(suffix)

	// First login: not in the group, lands on the default role.
	user, err := server.findOrCreateOAuthUser(ctx, provider, oauthUser)
	require.NoError(t, err)
	assert.Equal(t, []string{store.RoleConnector}, []string(user.Roles))

	// Second login, now a member of db-admins.
	promoted, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)
	assert.Equal(t, user.UID, promoted.UID, "the same account, not a new one")
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(promoted.Roles))

	// And it is persisted, not just returned.
	stored, err := dataStore.GetUserByUID(ctx, user.UID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(stored.Roles))

	events := roleSyncEvents(t, dataStore, user.UID)
	require.Len(t, events, 1, "the promotion must leave an audit trail")

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, []any{"admin"}, details["granted"])
	assert.Equal(t, []any{"db-admins"}, details["groups"])
	assert.Equal(t, oidc.ProviderName, details["provider"])
}

// TestOAuthRoleSyncDemotesWhenGroupIsLost is the case the whole spec exists
// for: leaving the team removes the role at the next login, with nobody
// remembering to do it.
func TestOAuthRoleSyncDemotesWhenGroupIsLost(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins")

	// A second admin, so the last-admin guard is not what is being measured.
	createTestUser(t, dataStore, "other-admin-"+suffix, "password", []string{store.RoleAdmin})

	provider := &mockProvider{name: oidc.ProviderName}

	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(user.Roles))

	// Next login, no longer a member.
	demoted, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix))
	require.NoError(t, err)
	assert.Equal(t, []string{store.RoleConnector}, []string(demoted.Roles))

	stored, err := dataStore.GetUserByUID(ctx, user.UID)
	require.NoError(t, err)
	assert.Equal(t, []string{store.RoleConnector}, []string(stored.Roles))

	events := roleSyncEvents(t, dataStore, user.UID)
	require.Len(t, events, 1, "the demotion must leave an audit trail")

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, []any{"admin"}, details["revoked"])
}

// TestOAuthRoleSyncRefusesToStripTheLastAdmin: a mapping is an external input,
// and an IdP outage or a renamed group must not be able to lock everyone out
// of dbbat. Same rule PATCH /users/:uid enforces, applied where no human is
// in the loop to see the 409.
func TestOAuthRoleSyncRefusesToStripTheLastAdmin(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins")

	provider := &mockProvider{name: oidc.ProviderName}

	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(user.Roles))

	adminCount, err := dataStore.CountAdmins(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, adminCount, "this test is only meaningful with a single admin")

	// The group is gone from the claim — but this is the only admin left.
	kept, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(kept.Roles),
		"the last admin keeps the role rather than locking the instance out")

	stored, err := dataStore.GetUserByUID(ctx, user.UID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(stored.Roles))

	assert.Empty(t, roleSyncEvents(t, dataStore, user.UID),
		"nothing changed, so nothing is audited")
}

// TestOAuthRoleSyncKeepsManuallyGrantedRoles pins the scope decision: the
// mapping owns the roles it names and nothing else. A role an admin granted in
// the UI must survive every login, or manual grants would be unusable
// alongside SSO.
func TestOAuthRoleSyncKeepsManuallyGrantedRoles(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	// The mapping names admin only; viewer is left to human judgement.
	serverWithRoleMapping(t, server, "admin=db-admins")
	createTestUser(t, dataStore, "other-admin-"+suffix, "password", []string{store.RoleAdmin})

	provider := &mockProvider{name: oidc.ProviderName}

	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)

	// An admin hands out viewer by hand, the way the users page does.
	require.NoError(t, dataStore.UpdateUser(ctx, user.UID, store.UserUpdate{
		Roles: store.StringArray{store.RoleConnector, store.RoleAdmin, store.RoleViewer},
	}))

	// Next login: still in db-admins, so nothing should move at all.
	same, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{store.RoleConnector, store.RoleAdmin, store.RoleViewer},
		[]string(same.Roles))

	// And a login that drops the mapped role keeps the hand-granted one.
	demoted, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix))
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{store.RoleConnector, store.RoleViewer},
		[]string(demoted.Roles),
		"viewer was never named by the mapping, so the login must not revoke it")
}

// TestOAuthRoleSyncAppliesAtCreation: a first login already resolves through
// the mapping, so a new engineer in db-admins is an admin immediately rather
// than after a second sign-in.
func TestOAuthRoleSyncAppliesAtCreation(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins,viewer=analysts")

	provider := &mockProvider{name: oidc.ProviderName}

	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "analysts"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleViewer}, []string(user.Roles),
		"the default role is the floor, the mapping adds to it")
}

// TestOAuthRoleSyncAuditsAutoCreation: with a mapping in play, a *first*
// login can mint an account that is admin outright. "No account → admin" is
// the transition an auditor goes looking for, so it has to be in audit_log,
// not only in a log line.
func TestOAuthRoleSyncAuditsAutoCreation(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins")

	provider := &mockProvider{name: oidc.ProviderName}

	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "db-admins"))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(user.Roles))

	events := auditEventsFor(t, dataStore, AuditEventUserCreated, user.UID)
	require.Len(t, events, 1, "auto-provisioning an account must be audited")

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.ElementsMatch(t, []any{store.RoleConnector, store.RoleAdmin}, details["roles"],
		"the entry must carry the roles the account was actually born with")
	assert.Equal(t, []any{"db-admins"}, details["groups"])
	assert.Equal(t, oidc.ProviderName, details["provider"])
	assert.Equal(t, true, details["auto_created"])
	assert.Nil(t, events[0].PerformedBy, "no dbbat user created this account")
}

// TestOAuthRoleSyncFloorIsNotAGrant covers the deployment where the default
// role is itself named in the mapping. An unmatched user has it dropped and
// handed back by the floor, which must read as "nothing happened": otherwise
// every single login writes identical roles and appends an audit entry
// claiming a promotion that never occurred — noise in a hash-chained log.
func TestOAuthRoleSyncFloorIsNotAGrant(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMappingAndDefault(t, server,
		"admin=db-admins,viewer=analysts", store.RoleViewer)

	provider := &mockProvider{name: oidc.ProviderName}

	// Created in analysts, so the account starts on viewer.
	user, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "analysts"))
	require.NoError(t, err)
	require.Equal(t, []string{store.RoleViewer}, []string(user.Roles))

	// Now in neither group: viewer is mapped and unmatched, so it is dropped
	// and then restored by the floor. Net effect, nothing.
	for range 3 {
		resolved, syncErr := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix))
		require.NoError(t, syncErr)
		assert.Equal(t, []string{store.RoleViewer}, []string(resolved.Roles))
	}

	assert.Empty(t, roleSyncEvents(t, dataStore, user.UID),
		"a round trip back to the same role set is not a change and must not be audited")
}

// TestOAuthRoleSyncIgnoresOtherProviders: the mapping is configured under
// DBB_OIDC_* and only the OIDC provider vouches for groups. A Slack login
// carries none, and reading that as "member of nothing" would revoke every
// mapped role on the way in.
func TestOAuthRoleSyncIgnoresOtherProviders(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "admin=db-admins")
	createTestUser(t, dataStore, "other-admin-"+suffix, "password", []string{store.RoleAdmin})

	user := createTestUser(t, dataStore, "slack-user-"+suffix, "password",
		[]string{store.RoleConnector, store.RoleAdmin})

	_, err := dataStore.CreateUserIdentity(ctx, &store.UserIdentity{
		UserID:     user.UID,
		Provider:   store.IdentityTypeSlack,
		ProviderID: "USLACK-" + suffix,
	})
	require.NoError(t, err)

	slackProvider := &mockProvider{name: store.IdentityTypeSlack}

	resolved, err := server.findOrCreateOAuthUser(ctx, slackProvider, &auth.OAuthUser{
		ProviderID: "USLACK-" + suffix,
		Email:      "slack-" + suffix + "@example.com",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(resolved.Roles),
		"a provider that cannot vouch for groups must not move roles")
}

// TestOAuthRoleSyncIsInertWithoutAMapping: the feature is opt-in. With no
// DBB_OIDC_ROLE_MAPPING, a login must never write to the roles column, even
// when the ID token happens to carry groups.
func TestOAuthRoleSyncIsInertWithoutAMapping(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	serverWithRoleMapping(t, server, "")
	createTestUser(t, dataStore, "other-admin-"+suffix, "password", []string{store.RoleAdmin})

	user := createTestUser(t, dataStore, "oidc-user-"+suffix, "password",
		[]string{store.RoleConnector, store.RoleAdmin})

	_, err := dataStore.CreateUserIdentity(ctx, &store.UserIdentity{
		UserID:     user.UID,
		Provider:   oidc.ProviderName,
		ProviderID: "OIDC-" + suffix,
	})
	require.NoError(t, err)

	provider := &mockProvider{name: oidc.ProviderName}

	resolved, err := server.findOrCreateOAuthUser(ctx, provider, oidcOAuthUser(suffix, "marketing"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{store.RoleConnector, store.RoleAdmin}, []string(resolved.Roles))
	assert.Empty(t, roleSyncEvents(t, dataStore, user.UID))
}
