package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/store"
)

// AuditEventUserIdentityUnlinked is written when a user detaches an SSO
// identity from their own account.
const AuditEventUserIdentityUnlinked = "user.identity_unlinked"

// identityLinkMethodSession marks an identity linked from a signed-in session,
// as opposed to "email_match" at login.
const identityLinkMethodSession = "session"

// profilePath is where the link flow starts and ends.
const profilePath = "/profile"

// linkNonceCookie carries the nonce binding a link flow to the browser that
// started it. Scoped to the provider's callback path, HttpOnly, and SameSite=Lax
// so it rides the identity provider's top-level redirect back to us.
const linkNonceCookie = "dbbat_link_nonce"

// linkNonceHash is what the state row stores: the cookie itself never touches
// the database.
func linkNonceHash(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))

	return hex.EncodeToString(sum[:])
}

// setLinkNonceCookie sets (maxAge > 0) or clears (maxAge < 0) the link nonce.
func (s *Server) setLinkNonceCookie(c *gin.Context, providerName, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     linkNonceCookie,
		Value:    value,
		Path:     callbackPath(providerName),
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   requestScheme(c.Request) == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// linkNonceMatches reports whether the callback request carries the nonce
// cookie of the link flow that minted state, and clears the cookie. A state row
// without a nonce hash never matches: every link flow mints one.
func (s *Server) linkNonceMatches(c *gin.Context, providerName string, state *store.OAuthState) bool {
	want := stateMetadataOf(state).LinkNonceHash
	got, err := c.Cookie(linkNonceCookie)

	s.setLinkNonceCookie(c, providerName, "", -1)

	if want == "" || err != nil || got == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(linkNonceHash(got)), []byte(want)) == 1
}

// OAuthLinkResponse is the body of POST /auth/{provider}/link.
type OAuthLinkResponse struct {
	AuthorizeURL string `json:"authorize_url"`
}

// handleOAuthLink starts the authorization flow that attaches the provider's
// identity to the *current* account. POST /api/v1/auth/:provider/link
func (s *Server) handleOAuthLink(providerName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		currentUser := getCurrentUser(c)
		if currentUser == nil {
			writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "not authenticated")
			return
		}

		authorizeURL, ok := s.startOAuthFlow(c, providerName, profilePath, &currentUser.UID)
		if !ok {
			return
		}

		c.JSON(http.StatusOK, OAuthLinkResponse{AuthorizeURL: authorizeURL})
	}
}

// profileURL builds a URL on the profile page under the configured base path.
func (s *Server) profileURL(query string) string {
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	return baseURL + profilePath + "?" + query
}

func (s *Server) redirectProfileLinkError(c *gin.Context) {
	c.Redirect(http.StatusFound, s.profileURL("link_error="+url.QueryEscape(string(ErrCodeOAuthFailed))))
}

// completeIdentityLink finishes a link flow: it attaches oauthUser to userID,
// the user the state row was bound to, and sends the browser back to the
// profile page.
func (s *Server) completeIdentityLink(
	c *gin.Context,
	providerName string,
	userID uuid.UUID,
	oauthUser *auth.OAuthUser,
) {
	ctx := c.Request.Context()

	user, err := s.store.GetUserByUID(ctx, userID)
	if err != nil {
		s.logger.WarnContext(ctx, "identity link: bound user is gone",
			slog.String("provider", providerName), slog.Any("error", err))
		s.redirectProfileLinkError(c)

		return
	}

	existing, err := s.store.GetIdentityByProviderID(ctx, providerName, oauthUser.ProviderID)
	switch {
	case err == nil && existing.UserID != user.UID:
		writeError(c, http.StatusConflict, ErrCodeConflict,
			"this identity is already linked to another user")

		return
	case err == nil:
		// Already linked to this very account: nothing to do.
		c.Redirect(http.StatusFound, s.profileURL("linked="+url.QueryEscape(providerName)))

		return
	case !errors.Is(err, store.ErrIdentityNotFound):
		s.logger.ErrorContext(ctx, "identity link lookup failed", slog.Any("error", err))
		s.redirectProfileLinkError(c)

		return
	}

	// One identity per provider and account: refuse a second one rather than
	// silently stacking identities the user can no longer tell apart.
	mine, err := s.store.GetUserIdentities(ctx, user.UID)
	if err != nil {
		s.logger.ErrorContext(ctx, "identity link: listing identities failed", slog.Any("error", err))
		s.redirectProfileLinkError(c)

		return
	}

	for i := range mine {
		if mine[i].Provider == providerName {
			writeError(c, http.StatusConflict, ErrCodeConflict,
				"this account already has an identity for that provider; unlink it first")

			return
		}
	}

	if err := s.linkIdentity(ctx, user.UID, providerName, oauthUser); err != nil {
		s.logger.ErrorContext(ctx, "identity link failed", slog.Any("error", err))
		s.redirectProfileLinkError(c)

		return
	}

	s.auditIdentityLinkedBySession(ctx, user, providerName, oauthUser)

	c.Redirect(http.StatusFound, s.profileURL("linked="+url.QueryEscape(providerName)))
}

// auditIdentityLinkedBySession records an identity the user attached to their
// own account from a signed-in session.
func (s *Server) auditIdentityLinkedBySession(
	ctx context.Context,
	user *store.User,
	providerName string,
	oauthUser *auth.OAuthUser,
) {
	details, err := json.Marshal(map[string]any{
		"provider":    providerName,
		"provider_id": oauthUser.ProviderID,
		"email":       oauthUser.Email,
		"method":      identityLinkMethodSession,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "failed to encode identity link audit details", slog.Any("error", err))

		return
	}

	if err := s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   AuditEventUserIdentityLinked,
		UserID:      &user.UID,
		PerformedBy: &user.UID,
		Details:     details,
	}); err != nil {
		s.logger.WarnContext(ctx, "failed to record identity link audit event", slog.Any("error", err))
	}
}

// IdentityResponse is a linked identity as the profile page shows it.
type IdentityResponse struct {
	Provider    string `json:"provider"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// IdentitiesResponse is the body of GET /users/me/identities.
type IdentitiesResponse struct {
	Identities []IdentityResponse `json:"identities"`
}

// handleListMyIdentities lists the SSO identities linked to the current user.
// GET /api/v1/users/me/identities
func (s *Server) handleListMyIdentities(c *gin.Context) {
	currentUser := getCurrentUser(c)
	if currentUser == nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "not authenticated")
		return
	}

	identities, err := s.store.GetUserIdentities(c.Request.Context(), currentUser.UID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list identities")
		return
	}

	out := make([]IdentityResponse, 0, len(identities))
	for i := range identities {
		out = append(out, IdentityResponse{
			Provider:    identities[i].Provider,
			Email:       identities[i].Email,
			DisplayName: identities[i].DisplayName,
			CreatedAt:   identities[i].CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	c.JSON(http.StatusOK, IdentitiesResponse{Identities: out})
}

// handleUnlinkMyIdentity detaches the current user's identity for a provider.
// DELETE /api/v1/users/me/identities/:provider
func (s *Server) handleUnlinkMyIdentity(c *gin.Context) {
	ctx := c.Request.Context()

	currentUser := getCurrentUser(c)
	if currentUser == nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "not authenticated")
		return
	}

	providerName := c.Param("provider")

	identities, err := s.store.GetUserIdentities(ctx, currentUser.UID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list identities")
		return
	}

	var target *store.UserIdentity
	for i := range identities {
		if identities[i].Provider == providerName {
			target = &identities[i]
			break
		}
	}

	if target == nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "no identity linked for that provider")
		return
	}

	if len(identities) == 1 && !hasLocalPassword(currentUser) {
		writeError(c, http.StatusConflict, ErrCodeConflict,
			"cannot unlink your only sign-in method: set a password or link another identity first")
		return
	}

	if err := s.store.DeleteUserIdentity(ctx, target.UID); err != nil {
		writeInternalError(c, s.logger, err, "failed to unlink identity")
		return
	}

	details, err := json.Marshal(map[string]any{
		"provider":    providerName,
		"provider_id": target.ProviderID,
	})
	if err == nil {
		if auditErr := s.store.LogAuditEvent(ctx, &store.AuditEvent{
			EventType:   AuditEventUserIdentityUnlinked,
			UserID:      &currentUser.UID,
			PerformedBy: &currentUser.UID,
			Details:     details,
		}); auditErr != nil {
			s.logger.WarnContext(ctx, "failed to record identity unlink audit event", slog.Any("error", auditErr))
		}
	}

	c.Status(http.StatusNoContent)
}
