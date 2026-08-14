package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// CreateServerGroupRequest is the body for POST /server-groups.
type CreateServerGroupRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	// MemberUIDs, when non-nil, replaces the group's membership. On create it
	// seeds it; on update a nil value leaves membership untouched.
	MemberUIDs []uuid.UUID `json:"member_uids"`
	// AccessApproverUserGroupUIDs / QueryApproverUserGroupUIDs are the
	// group-level fallback for the two approver kinds: they cover every member
	// server that names no approvers of its own. Several groups holding the same
	// server union. Empty means the group names nobody, which leaves the
	// decision to admins.
	//
	// Unlike member_uids these are written on every update, empty included —
	// clearing a list is a real policy change and has to be expressible.
	AccessApproverUserGroupUIDs []uuid.UUID `json:"access_approver_user_group_uids"`
	QueryApproverUserGroupUIDs  []uuid.UUID `json:"query_approver_user_group_uids"`
}

// UpdateServerGroupRequest is the body for PATCH /server-groups/:uid. Same
// shape as create — this surface is too small to warrant a separate partial
// type, exactly like the user-group one.
type UpdateServerGroupRequest = CreateServerGroupRequest

func validateServerGroupRequest(req *CreateServerGroupRequest) string {
	if req.Name == "" {
		return "name is required"
	}

	if len(req.Name) > maxGroupNameLen {
		return "name must be at most 64 characters"
	}

	return ""
}

// checkServersExist verifies every uid maps to a real database target, so
// membership can never reference a ghost — nor an SSH bastion, which is a dial
// path rather than something access is granted on.
func (s *Server) checkServersExist(c *gin.Context, uids []uuid.UUID) string {
	for _, uid := range uids {
		target, err := s.store.GetServerByUID(c.Request.Context(), uid)
		if err != nil {
			return "server does not exist: " + uid.String()
		}

		if target.IsTunnel() {
			return "cannot add a " + target.Protocol + " tunnel server to a server group: " + uid.String()
		}
	}

	return ""
}

// serverGroupResponse is a group plus its resolved membership and the number
// of live grants bound to it.
//
// ActiveGrantCount is not decoration: membership is live, so adding a server
// widens every one of those grants the instant it is saved. The admin UI shows
// this count in its warning before the edit is committed.
type serverGroupResponse struct {
	*store.ServerGroup

	MemberUIDs       []uuid.UUID `json:"member_uids"`
	ActiveGrantCount int64       `json:"active_grant_count"`
}

func (s *Server) serverGroupWithMembers(c *gin.Context, group *store.ServerGroup) (*serverGroupResponse, error) {
	ctx := c.Request.Context()

	members, err := s.store.ListServerGroupMemberUIDs(ctx, group.UID)
	if err != nil {
		return nil, err
	}

	count, err := s.store.CountActiveGrantsForServerGroup(ctx, group.UID)
	if err != nil {
		return nil, err
	}

	return &serverGroupResponse{ServerGroup: group, MemberUIDs: members, ActiveGrantCount: count}, nil
}

// handleCreateServerGroup — admin-only.
func (s *Server) handleCreateServerGroup(c *gin.Context) {
	var req CreateServerGroupRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateServerGroupRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.checkServersExist(c, req.MemberUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	currentUser := getCurrentUser(c)
	ctx := c.Request.Context()

	if msg := s.validateApproverUserGroups(ctx,
		req.AccessApproverUserGroupUIDs, req.QueryApproverUserGroupUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	created, err := s.store.CreateServerGroup(ctx, &store.ServerGroup{
		Name:                        req.Name,
		Description:                 req.Description,
		AccessApproverUserGroupUIDs: normalizeUUIDs(req.AccessApproverUserGroupUIDs),
		QueryApproverUserGroupUIDs:  normalizeUUIDs(req.QueryApproverUserGroupUIDs),
		CreatedBy:                   &currentUser.UID,
	})
	if err != nil {
		if errors.Is(err, store.ErrServerGroupDuplicate) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())

			return
		}

		writeInternalError(c, s.logger, err, "failed to create server group")

		return
	}

	if len(req.MemberUIDs) > 0 {
		if err := s.store.SetServerGroupMembers(ctx, created.UID, req.MemberUIDs); err != nil {
			writeInternalError(c, s.logger, err, "failed to set server group members")

			return
		}
	}

	details, _ := json.Marshal(map[string]any{
		"server_group_uid":                created.UID,
		"name":                            created.Name,
		"member_uids":                     req.MemberUIDs,
		"access_approver_user_group_uids": created.AccessApproverUserGroupUIDs,
		"query_approver_user_group_uids":  created.QueryApproverUserGroupUIDs,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "server_group.created",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	resp, err := s.serverGroupWithMembers(c, created)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load server group members")

		return
	}

	successResponse(c, resp)
}

// handleListServerGroups — admin-only. Server groups are an access-control
// surface, so they stay behind the admin gate like user groups.
func (s *Server) handleListServerGroups(c *gin.Context) {
	groups, err := s.store.ListServerGroups(c.Request.Context())
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list server groups")

		return
	}

	out := make([]*serverGroupResponse, 0, len(groups))

	for i := range groups {
		resp, err := s.serverGroupWithMembers(c, &groups[i])
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to load server group members")

			return
		}

		out = append(out, resp)
	}

	successResponse(c, gin.H{"server_groups": out})
}

// handleGetServerGroup — admin-only; returns the group plus its members.
func (s *Server) handleGetServerGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid server group UID")

		return
	}

	group, err := s.store.GetServerGroup(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get server group")

		return
	}

	resp, err := s.serverGroupWithMembers(c, group)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load server group members")

		return
	}

	successResponse(c, resp)
}

// handleUpdateServerGroup — admin-only.
//
// Membership is live: replacing it here immediately changes what every grant
// bound to this group covers, with no versioning in between. That is the
// deliberate exception to dbbat's "a live grant's behavior never changes
// under it" rule, so the change is recorded as its own audit event carrying
// the resulting membership.
func (s *Server) handleUpdateServerGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid server group UID")

		return
	}

	var req UpdateServerGroupRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateServerGroupRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.checkServersExist(c, req.MemberUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	ctx := c.Request.Context()

	group, err := s.store.GetServerGroup(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get server group")

		return
	}

	if msg := s.validateApproverUserGroups(ctx,
		req.AccessApproverUserGroupUIDs, req.QueryApproverUserGroupUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	group.Name = req.Name
	group.Description = req.Description
	// Live, like membership: this immediately changes who may decide grant
	// requests and holds for every member server that names no approvers of its
	// own — including requests already filed and statements already parked.
	group.AccessApproverUserGroupUIDs = normalizeUUIDs(req.AccessApproverUserGroupUIDs)
	group.QueryApproverUserGroupUIDs = normalizeUUIDs(req.QueryApproverUserGroupUIDs)

	if err := s.store.UpdateServerGroup(ctx, group); err != nil {
		if errors.Is(err, store.ErrServerGroupDuplicate) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())

			return
		}

		writeInternalError(c, s.logger, err, "failed to update server group")

		return
	}

	// A nil member_uids means "don't touch membership"; an explicit (even
	// empty) list replaces it wholesale.
	if req.MemberUIDs != nil {
		if err := s.store.SetServerGroupMembers(ctx, group.UID, req.MemberUIDs); err != nil {
			writeInternalError(c, s.logger, err, "failed to set server group members")

			return
		}
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"server_group_uid":                group.UID,
		"name":                            group.Name,
		"member_uids":                     req.MemberUIDs,
		"access_approver_user_group_uids": group.AccessApproverUserGroupUIDs,
		"query_approver_user_group_uids":  group.QueryApproverUserGroupUIDs,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "server_group.updated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	resp, err := s.serverGroupWithMembers(c, group)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load server group members")

		return
	}

	successResponse(c, resp)
}

// handleDeleteServerGroup — admin-only hard delete. Memberships cascade away
// and grants bound to the group fall back to their anchor database; grant
// definitions scoped to it keep the now-dangling uid and therefore match no
// database (fail closed) until an admin edits them.
func (s *Server) handleDeleteServerGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid server group UID")

		return
	}

	ctx := c.Request.Context()

	if err := s.store.DeleteServerGroup(ctx, uid); err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to delete server group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{"server_group_uid": uid})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "server_group.deleted",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "server group deleted"})
}

// parseServerMemberParams pulls the (group uid, server uid) pair out of the URL.
func parseServerMemberParams(c *gin.Context) (uuid.UUID, uuid.UUID, error) {
	groupUID, err := uuid.Parse(c.Param("uid"))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidUID
	}

	serverUID, err := uuid.Parse(c.Param("server_uid"))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidUID
	}

	return groupUID, serverUID, nil
}

// handleListServerGroupMembers — admin-only; full server rows for the admin UI.
func (s *Server) handleListServerGroupMembers(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid server group UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetServerGroup(ctx, uid); err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get server group")

		return
	}

	servers, err := s.store.ListServerGroupMembers(ctx, uid)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list server group members")

		return
	}

	out := make([]DatabaseLimitedResponse, 0, len(servers))
	for i := range servers {
		out = append(out, toDatabaseLimitedResponse(&servers[i]))
	}

	successResponse(c, gin.H{"servers": out})
}

// handleAddServerGroupMember — admin-only. Idempotent.
func (s *Server) handleAddServerGroupMember(c *gin.Context) {
	groupUID, serverUID, err := parseServerMemberParams(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetServerGroup(ctx, groupUID); err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get server group")

		return
	}

	if msg := s.checkServersExist(c, []uuid.UUID{serverUID}); msg != "" {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, msg)

		return
	}

	if err := s.store.AddServerToGroup(ctx, groupUID, serverUID); err != nil {
		writeInternalError(c, s.logger, err, "failed to add server to group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"server_group_uid": groupUID,
		"server_uid":       serverUID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "server_group.member_added",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "member added"})
}

// handleRemoveServerGroupMember — admin-only. Idempotent.
func (s *Server) handleRemoveServerGroupMember(c *gin.Context) {
	groupUID, serverUID, err := parseServerMemberParams(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetServerGroup(ctx, groupUID); err != nil {
		if errors.Is(err, store.ErrServerGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "server group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get server group")

		return
	}

	if err := s.store.RemoveServerFromGroup(ctx, groupUID, serverUID); err != nil {
		writeInternalError(c, s.logger, err, "failed to remove server from group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"server_group_uid": groupUID,
		"server_uid":       serverUID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "server_group.member_removed",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "member removed"})
}
