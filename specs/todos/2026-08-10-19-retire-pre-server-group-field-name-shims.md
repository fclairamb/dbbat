---
model: sonnet
effort: medium
---

# Retire the pre-rename `group_uids` / `approver_group_uids` input shims

**No GitHub issue filed yet — one should be.** (The server-groups work that
introduced these shims could not file one either; the repository owner has not
authorised outward-facing actions from the implementing agent.)

## Goal

Delete the one-release compatibility window that lets `/api/v1` still *read*
the pre-rename JSON field names, one release after
`2026-08-09-rights-through-server-groups` ships.

## Why

Server groups made a bare "group" ambiguous, so `group_uids` became
`user_group_uids` and `approver_group_uids` became
`approver_user_group_uids`. Responses were switched immediately; input kept
accepting the old spellings for exactly one release so existing API clients
would not break the day the rename landed. That window is a promise with an
end date — leaving it open indefinitely means two spellings of every field
forever, which is the ambiguity the rename existed to remove.

## Implementation

Remove, in this order:

- `LegacyUserGroupUIDs` / `LegacyApproverUserGroupUIDs` and both
  `applyLegacyFields` methods on `CreateGrantDefinitionRequest` and
  `UpdateGrantDefinitionRequest`
  ([internal/api/grant_definitions.go](internal/api/grant_definitions.go)),
  plus the two `req.applyLegacyFields()` calls in the handlers.
- `UpdateUserRequest.LegacyUserGroupUIDs` and its `applyLegacyFields`
  ([internal/api/users.go](internal/api/users.go)).
- The `deprecated: true` `group_uids` / `approver_group_uids` properties on
  `CreateGrantDefinitionRequest`, `UpdateGrantDefinitionRequest` and
  `UpdateUserRequest` in [internal/api/openapi.yml](internal/api/openapi.yml),
  then `cd front && bun run generate-client`.
- `TestGrantDefinition_LegacyGroupFieldNamesAccepted` and
  `TestGrantDefinition_NewGroupFieldNamesWinOverLegacy`
  ([internal/api/grant_definitions_rename_test.go](internal/api/grant_definitions_rename_test.go))
  — replace them with one test asserting the old names are now *ignored*
  (they decode into nothing), or a 400 if that reads better. Ignoring a user
  group scope fails **open**, so prefer the 400, matching how
  `database_uids` is refused today (`errRetiredDatabaseUIDs`).

Note the removal in the conventional-commit body as a `BREAKING CHANGE:` —
release-please owns `CHANGELOG.md`.

## Don't do this early

Check `.release-please-manifest.json`: the shims must survive at least one
tagged release after the one that introduced them.
