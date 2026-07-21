package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// maxGroupNameLen bounds group names so they stay readable in the grant
// definition editor and in Slack notifications.
const maxGroupNameLen = 64

// CreateUserGroupRequest is the body for POST /user-groups.
type CreateUserGroupRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	// MemberUIDs, when non-nil, replaces the group's membership. On create
	// it seeds it; on update a nil value leaves membership untouched.
	MemberUIDs []uuid.UUID `json:"member_uids"`
}

// UpdateUserGroupRequest is the body for PATCH /user-groups/:uid. Same shape
// as create — this surface is too small to warrant a separate partial type.
type UpdateUserGroupRequest = CreateUserGroupRequest

func validateUserGroupRequest(req *CreateUserGroupRequest) string {
	if req.Name == "" {
		return "name is required"
	}

	if len(req.Name) > maxGroupNameLen {
		return "name must be at most 64 characters"
	}

	return ""
}

// checkUsersExist verifies every uid maps to a real user, so membership can
// never reference a ghost. Returns a validation message, or "" when fine.
func (s *Server) checkUsersExist(c *gin.Context, uids []uuid.UUID) string {
	for _, uid := range uids {
		if _, err := s.store.GetUserByUID(c.Request.Context(), uid); err != nil {
			return "user does not exist: " + uid.String()
		}
	}

	return ""
}

// userGroupResponse is a group plus its resolved membership, which is what
// both the admin list and the group detail view need.
type userGroupResponse struct {
	*store.UserGroup

	MemberUIDs []uuid.UUID `json:"member_uids"`
}

func (s *Server) groupWithMembers(c *gin.Context, group *store.UserGroup) (*userGroupResponse, error) {
	members, err := s.store.ListGroupMemberUIDs(c.Request.Context(), group.UID)
	if err != nil {
		return nil, err
	}

	return &userGroupResponse{UserGroup: group, MemberUIDs: members}, nil
}

// handleCreateUserGroup — admin-only.
func (s *Server) handleCreateUserGroup(c *gin.Context) {
	var req CreateUserGroupRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateUserGroupRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.checkUsersExist(c, req.MemberUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	currentUser := getCurrentUser(c)
	ctx := c.Request.Context()

	created, err := s.store.CreateUserGroup(ctx, &store.UserGroup{
		Name:        req.Name,
		Description: req.Description,
		CreatedBy:   &currentUser.UID,
	})
	if err != nil {
		if errors.Is(err, store.ErrUserGroupDuplicate) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())

			return
		}

		writeInternalError(c, s.logger, err, "failed to create user group")

		return
	}

	if len(req.MemberUIDs) > 0 {
		if err := s.store.SetGroupMembers(ctx, created.UID, req.MemberUIDs); err != nil {
			writeInternalError(c, s.logger, err, "failed to set group members")

			return
		}
	}

	details, _ := json.Marshal(map[string]any{
		"user_group_uid": created.UID,
		"name":           created.Name,
		"member_uids":    req.MemberUIDs,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "user_group.created",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	resp, err := s.groupWithMembers(c, created)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load group members")

		return
	}

	successResponse(c, resp)
}

// handleListUserGroups — admin-only. Groups are an access-control surface, so
// they stay behind the admin gate like grant definition management.
func (s *Server) handleListUserGroups(c *gin.Context) {
	groups, err := s.store.ListUserGroups(c.Request.Context())
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list user groups")

		return
	}

	out := make([]*userGroupResponse, 0, len(groups))

	for i := range groups {
		resp, err := s.groupWithMembers(c, &groups[i])
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to load group members")

			return
		}

		out = append(out, resp)
	}

	successResponse(c, gin.H{"user_groups": out})
}

// handleGetUserGroup — admin-only; returns the group plus its members.
func (s *Server) handleGetUserGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user group UID")

		return
	}

	group, err := s.store.GetUserGroup(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get user group")

		return
	}

	resp, err := s.groupWithMembers(c, group)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load group members")

		return
	}

	successResponse(c, resp)
}

// handleUpdateUserGroup — admin-only.
func (s *Server) handleUpdateUserGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user group UID")

		return
	}

	var req UpdateUserGroupRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateUserGroupRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.checkUsersExist(c, req.MemberUIDs); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	ctx := c.Request.Context()

	group, err := s.store.GetUserGroup(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get user group")

		return
	}

	group.Name = req.Name
	group.Description = req.Description

	if err := s.store.UpdateUserGroup(ctx, group); err != nil {
		if errors.Is(err, store.ErrUserGroupDuplicate) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())

			return
		}

		writeInternalError(c, s.logger, err, "failed to update user group")

		return
	}

	// A nil member_uids means "don't touch membership"; an explicit (even
	// empty) list replaces it wholesale.
	if req.MemberUIDs != nil {
		if err := s.store.SetGroupMembers(ctx, group.UID, req.MemberUIDs); err != nil {
			writeInternalError(c, s.logger, err, "failed to set group members")

			return
		}
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"user_group_uid": group.UID,
		"name":           group.Name,
		"member_uids":    req.MemberUIDs,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "user_group.updated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	resp, err := s.groupWithMembers(c, group)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to load group members")

		return
	}

	successResponse(c, resp)
}

// handleDeleteUserGroup — admin-only hard delete. Definitions scoped to the
// group keep the now-dangling uid and therefore match nobody (fail closed).
func (s *Server) handleDeleteUserGroup(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user group UID")

		return
	}

	ctx := c.Request.Context()

	if err := s.store.DeleteUserGroup(ctx, uid); err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to delete user group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{"user_group_uid": uid})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "user_group.deleted",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "user group deleted"})
}

// parseMemberParams pulls the (group uid, user uid) pair out of the URL.
func parseMemberParams(c *gin.Context) (groupUID, userUID uuid.UUID, err error) {
	groupUID, err = uuid.Parse(c.Param("uid"))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidUID
	}

	userUID, err = uuid.Parse(c.Param("user_uid"))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidUID
	}

	return groupUID, userUID, nil
}

// handleAddUserGroupMember — admin-only. Idempotent.
func (s *Server) handleAddUserGroupMember(c *gin.Context) {
	groupUID, userUID, err := parseMemberParams(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get user group")

		return
	}

	if _, err := s.store.GetUserByUID(ctx, userUID); err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")

		return
	}

	if err := s.store.AddUserToGroup(ctx, groupUID, userUID); err != nil {
		writeInternalError(c, s.logger, err, "failed to add user to group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"user_group_uid": groupUID,
		"user_uid":       userUID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "user_group.member_added",
		UserID:      &userUID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "member added"})
}

// handleRemoveUserGroupMember — admin-only. Idempotent.
func (s *Server) handleRemoveUserGroupMember(c *gin.Context) {
	groupUID, userUID, err := parseMemberParams(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get user group")

		return
	}

	if err := s.store.RemoveUserFromGroup(ctx, groupUID, userUID); err != nil {
		writeInternalError(c, s.logger, err, "failed to remove user from group")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"user_group_uid": groupUID,
		"user_uid":       userUID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "user_group.member_removed",
		UserID:      &userUID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "member removed"})
}

// handleListUserGroupMembers — admin-only; full user rows for the admin UI.
func (s *Server) handleListUserGroupMembers(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user group UID")

		return
	}

	ctx := c.Request.Context()

	if _, err := s.store.GetUserGroup(ctx, uid); err != nil {
		if errors.Is(err, store.ErrUserGroupNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user group not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get user group")

		return
	}

	users, err := s.store.ListGroupMembers(ctx, uid)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list group members")

		return
	}

	successResponse(c, gin.H{"users": users})
}
