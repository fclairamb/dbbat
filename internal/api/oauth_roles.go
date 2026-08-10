package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/auth/oidc"
	"github.com/fclairamb/dbbat/internal/store"
)

// AuditEventOAuthRolesSynced is written whenever a login changes a user's
// roles because their directory membership changed. It is the trail an auditor
// follows to answer "who made this person an admin, and when did they stop
// being one" once the answer is "the identity provider did".
const AuditEventOAuthRolesSynced = "user.roles_synced"

// AuditEventUserCreated is written when an account comes into existence,
// whether an admin created it through the users API or a verified OAuth
// identity auto-provisioned it. Deliberately the same event type for both:
// "where did this account come from" is one question, and it should not need
// two queries to answer. The details say which path it took.
const AuditEventUserCreated = "user.created"

// oauthDefaultRole is the single place the auto-provisioning default role is
// read. It is the floor a role mapping can never dig below: a mapping that
// matches nothing leaves the user with this role, never with none at all.
//
// The value is DBB_AUTH_DEFAULT_ROLE, provider-agnostic on purpose: whichever
// provider vouched for the identity, this is the role it lands on.
func (s *Server) oauthDefaultRole() string {
	if s.config == nil {
		return store.RoleConnector
	}

	return s.config.Auth.Role()
}

// oauthAutoCreateUsers reports whether an unknown verified identity may
// provision itself a local account (DBB_AUTH_AUTO_CREATE_USERS). Same
// provider-agnostic home as the default role, same single-read discipline.
func (s *Server) oauthAutoCreateUsers() bool {
	return s.config != nil && s.config.Auth.AutoCreateUsers
}

// oauthRoleMapping returns the parsed group-to-role mapping that applies to
// providerName, or nil when none does.
//
// The mapping is deliberately scoped to the generic OIDC provider: it is
// configured under DBB_OIDC_*, and it is the only provider that vouches for
// group membership. Applying it to a Slack login — which never carries groups —
// would read as "this user is in no groups" and revoke every mapped role.
//
// The value is re-parsed per login rather than cached: it is a handful of
// bytes, it was already validated at startup (Load refuses a malformed one),
// and a parse failure here fails closed by returning nothing, which leaves
// roles exactly as they are.
func (s *Server) oauthRoleMapping(ctx context.Context, providerName string) map[string][]string {
	if s.config == nil || providerName != oidc.ProviderName {
		return nil
	}

	mapping, err := s.config.OIDCAuth.ParseRoleMapping()
	if err != nil {
		// Unreachable in a process that booted, since Load validates it.
		s.logger.ErrorContext(ctx, "OIDC role mapping is malformed, leaving roles untouched",
			slog.Any("error", err))

		return nil
	}

	return mapping
}

// roleResolution is the outcome of applying a group mapping to a user's
// current roles.
type roleResolution struct {
	// Roles is the resolved set, in a stable order.
	Roles []string
	// Granted and Revoked are the difference against the current roles, kept
	// for the audit entry.
	Granted []string
	Revoked []string
}

// changed reports whether the resolution moves anything.
func (r roleResolution) changed() bool {
	return len(r.Granted) > 0 || len(r.Revoked) > 0
}

// resolveMappedRoles computes the roles a user should hold after a login,
// given their directory groups.
//
// The mapping is authoritative **only for the roles it names**. A role nobody
// mapped — one an admin granted by hand in the UI — is never touched by a
// login, because the directory has no opinion about it and silently revoking
// it would make manual grants unusable. A role the mapping *does* name is
// fully directory-controlled: matched groups grant it, and the absence of a
// matching group revokes it. That revocation is the entire point of the
// feature — the admin who leaves the team loses the role at their next login
// without anyone remembering to click.
//
// Group values are matched exactly, case included: a mapping that does not
// match is a mapping that grants nothing, which is the safe direction to fail.
//
// defaultRole is the floor. If everything above resolves to an empty set, the
// user keeps that one role rather than ending up with none — a user with no
// roles can sign in and do nothing, which is a lockout, not a demotion.
//
// forced names roles to treat as matched whatever the directory says. It
// exists for exactly one caller: the last-admin guard, which re-runs the
// resolution with "admin" pinned rather than patching the result afterwards.
func resolveMappedRoles(
	current, groups []string,
	mapping map[string][]string,
	defaultRole string,
	forced ...string,
) roleResolution {
	if defaultRole == "" {
		defaultRole = store.RoleConnector
	}

	member := make(map[string]bool, len(groups))
	for _, g := range groups {
		member[g] = true
	}

	// Which mapped roles the directory says this user should have.
	matched := make(map[string]bool, len(mapping))

	for role, wanted := range mapping {
		for _, group := range wanted {
			if member[group] {
				matched[role] = true

				break
			}
		}
	}

	for _, role := range forced {
		if slices.Contains(current, role) {
			matched[role] = true
		}
	}

	next := make([]string, 0, len(current)+len(matched))

	// Keep the current roles the mapping has no opinion about, plus the mapped
	// ones still matched. Order is preserved so an unchanged set stays byte
	// -identical in the database.
	for _, role := range current {
		if _, mapped := mapping[role]; mapped && !matched[role] {
			continue
		}

		if !slices.Contains(next, role) {
			next = append(next, role)
		}
	}

	// Newly granted roles, appended in a deterministic order.
	granted := make([]string, 0, len(matched))

	for role := range matched {
		if !slices.Contains(next, role) {
			granted = append(granted, role)
		}
	}

	sort.Strings(granted)
	next = append(next, granted...)

	if len(next) == 0 {
		next = []string{defaultRole}

		// Only a grant if the user did not already hold it. The default role
		// can itself be named in the mapping ("viewer=analysts" with
		// DBB_AUTH_DEFAULT_ROLE=viewer): an unmatched user has it
		// dropped and then restored by the floor, which is a round trip back
		// to where they started — not a promotion. Recording it as one would
		// make changed() true on every single login, writing identical roles
		// and appending an audit entry claiming a grant that never happened.
		if !slices.Contains(current, defaultRole) {
			granted = append(granted, defaultRole)
		}
	}

	revoked := make([]string, 0)

	for _, role := range current {
		if !slices.Contains(next, role) {
			revoked = append(revoked, role)
		}
	}

	return roleResolution{Roles: next, Granted: granted, Revoked: revoked}
}

// oauthRolesForNewUser is resolveMappedRoles for an account that does not
// exist yet: it starts from the default role, so a first login through the
// directory lands on the right roles immediately instead of on the default
// until the second one.
func (s *Server) oauthRolesForNewUser(
	ctx context.Context,
	providerName string,
	oauthUser *auth.OAuthUser,
) []string {
	role := s.oauthDefaultRole()

	mapping := s.oauthRoleMapping(ctx, providerName)
	if len(mapping) == 0 {
		return []string{role}
	}

	return resolveMappedRoles([]string{role}, oauthUser.Groups, mapping, role).Roles
}

// syncOAuthRoles applies the directory mapping to an existing user, on **every**
// login rather than only at creation. Returns the user with its refreshed
// roles.
//
// Failures are returned, not swallowed: refusing the login is the fail-closed
// outcome when we cannot establish what the user is entitled to. The audit
// write is the exception — losing the trail must not cost the user their
// session — but it is logged loudly.
func (s *Server) syncOAuthRoles(
	ctx context.Context,
	user *store.User,
	providerName string,
	oauthUser *auth.OAuthUser,
) (*store.User, error) {
	mapping := s.oauthRoleMapping(ctx, providerName)
	if len(mapping) == 0 {
		return user, nil
	}

	if len(oauthUser.Groups) == 0 {
		// Either the user really is in no groups, or the IdP was never
		// configured to emit the claim. Both revoke the mapped roles, so say
		// so out loud — this is the log line that explains a surprise
		// org-wide demotion.
		s.logger.WarnContext(ctx, "OIDC login carried no group claim; mapped roles will be revoked",
			slog.String("provider", providerName),
			slog.String("username", user.Username),
			slog.String("groups_claim", s.config.OIDCAuth.GroupsClaimName()))
	}

	resolution := resolveMappedRoles(user.Roles, oauthUser.Groups, mapping, s.oauthDefaultRole())

	if slices.Contains(resolution.Revoked, store.RoleAdmin) {
		retained, err := s.retainLastAdmin(ctx, user, oauthUser.Groups, mapping, resolution)
		if err != nil {
			return nil, err
		}

		resolution = retained
	}

	if !resolution.changed() {
		return user, nil
	}

	if err := s.store.UpdateUser(ctx, user.UID, store.UserUpdate{Roles: resolution.Roles}); err != nil {
		return nil, fmt.Errorf("sync roles from %s groups: %w", providerName, err)
	}

	s.logger.InfoContext(ctx, "roles synced from identity provider groups",
		slog.String("provider", providerName),
		slog.String("username", user.Username),
		slog.Any("uid", user.UID),
		slog.Any("granted", resolution.Granted),
		slog.Any("revoked", resolution.Revoked))

	s.auditRoleSync(ctx, user, providerName, oauthUser.Groups, resolution)

	user.Roles = store.StringArray(resolution.Roles)

	return user, nil
}

// retainLastAdmin puts the admin role back when revoking it would leave the
// instance with no admin at all — the same rule user management enforces on
// PATCH /users/:uid, applied here because a directory mapping can reach the
// same lockout without a human in the loop.
func (s *Server) retainLastAdmin(
	ctx context.Context,
	user *store.User,
	groups []string,
	mapping map[string][]string,
	resolution roleResolution,
) (roleResolution, error) {
	last, err := s.isLastAdmin(ctx, user)
	if err != nil {
		return resolution, fmt.Errorf("check last admin before role sync: %w", err)
	}

	if !last {
		return resolution, nil
	}

	s.logger.WarnContext(ctx, "keeping the admin role: the group mapping would have removed the last admin",
		slog.String("username", user.Username),
		slog.Any("uid", user.UID))

	// Re-resolve with admin pinned rather than patching the result: the floor
	// may have added the default role only because the set was about to be
	// empty, and that reasoning no longer holds once admin stays.
	return resolveMappedRoles(user.Roles, groups, mapping, s.oauthDefaultRole(), store.RoleAdmin), nil
}

// auditOAuthUserCreated records an account auto-provisioned by a verified
// OAuth identity.
//
// This matters more since roles started following directory groups: a *first*
// login can now mint an account that is `admin` outright, and "no account →
// admin" is precisely the transition an auditor goes looking for. Before the
// mapping, a new account always landed on the static default role and the slog
// line was arguably enough; it no longer is.
func (s *Server) auditOAuthUserCreated(
	ctx context.Context,
	user *store.User,
	providerName string,
	oauthUser *auth.OAuthUser,
) {
	details, err := json.Marshal(map[string]any{
		"username":     user.Username,
		"roles":        []string(user.Roles),
		"provider":     providerName,
		"email":        oauthUser.Email,
		"groups":       oauthUser.Groups,
		"auto_created": true,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "failed to encode OAuth user creation audit details", slog.Any("error", err))

		return
	}

	// PerformedBy stays nil: no dbbat user created this account, a verified
	// identity from providerName did.
	if err := s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: AuditEventUserCreated,
		UserID:    &user.UID,
		Details:   details,
	}); err != nil {
		s.logger.WarnContext(ctx, "failed to record OAuth user creation audit event",
			slog.String("username", user.Username),
			slog.Any("error", err))
	}
}

// auditRoleSync records a role change driven by directory membership. The
// group values are recorded verbatim: reconstructing why someone was promoted
// six months ago is impossible without them.
func (s *Server) auditRoleSync(
	ctx context.Context,
	user *store.User,
	providerName string,
	groups []string,
	resolution roleResolution,
) {
	details, err := json.Marshal(map[string]any{
		"provider":       providerName,
		"groups":         groups,
		"previous_roles": []string(user.Roles),
		"roles":          resolution.Roles,
		"granted":        resolution.Granted,
		"revoked":        resolution.Revoked,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "failed to encode role sync audit details", slog.Any("error", err))

		return
	}

	// PerformedBy stays nil on purpose: no dbbat user made this decision, the
	// identity provider did. The details name it.
	if err := s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType: AuditEventOAuthRolesSynced,
		UserID:    &user.UID,
		Details:   details,
	}); err != nil {
		s.logger.WarnContext(ctx, "failed to record role sync audit event",
			slog.String("username", user.Username),
			slog.Any("error", err))
	}
}
