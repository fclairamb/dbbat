# A Kubernetes row cannot change its bastion from the edit dialog

## Goal

Let `EditSSHServerForm` change a Kubernetes cluster row's `via_uid` — the SSH
bastion it is dialed through — the way the create dialog already lets one be
picked.

## Why

Noticed while adding the database edit form
(`specs/todos/2026-09-21-04-database-rows-have-no-edit-form-in-the-ui.md`).

`CreateDatabaseDialog` offers a "Via SSH bastion" selector for every non-SSH
row, Kubernetes clusters included: an API server reachable only through a jump
host is a real deployment, and the field exists for it. `EditSSHServerForm`,
which owns both tunnel kinds, offers name, description, host, port, username,
password and the kind-specific material — and no `via_uid` at all.

So the bastion in front of a cluster is set-once. Changing it means editing the
row through the API, or deleting and re-creating the cluster, which orphans
every database row that dials `via` it. The same argument the database edit
form made, one row-kind over.

`PUT /api/v1/servers/:uid` already accepts `via_uid` and `clear_via_uid`, and
`store.validateViaUID` already refuses the unsupported nestings (a cluster is
never offered as a cluster's own via), so this is a form field, not a new
capability.

## Implementation

- In `EditSSHServerForm`
  (`front/src/routes/_authenticated/servers/index.tsx`), render the same "Via
  SSH bastion" `Select` the create dialog does, under `isKubernetes`. Seed it
  from `server.via_uid`, and filter the options the create dialog's way —
  `useTunnelServers()` narrowed to `protocol === "ssh"`, minus the row being
  edited.
- Diff on submit like the database form does: a new value sends `via_uid`,
  clearing it sends `clear_via_uid: true` (an omitted `via_uid` leaves the
  tunnel alone), and an unchanged one sends neither — so the
  `database.updated` audit entry stays a list of real edits.
- SSH rows keep no via selector: the create dialog does not offer one either
  (`!isSSH`), and bastion-behind-bastion chaining is configured on the row that
  dials, not here.
- E2E: extend `front/e2e/servers.spec.ts` — create a bastion and a cluster,
  edit the cluster to dial through the bastion, reopen and confirm the
  selection; then clear it and confirm it goes back to direct.

## Implementation Plan

1. **`EditSSHServerForm`** (`front/src/routes/_authenticated/servers/index.tsx`):
   - Add `const [viaUid, setViaUid] = useState(server.via_uid || "")` and
     `const { data: tunnelServers } = useTunnelServers()`.
   - Build `viaOptions` = `tunnelServers` filtered to `protocol === "ssh"` and
     `uid !== server.uid` (a cluster is never dialed through itself, and the
     reverse nesting — a bastion behind a port-forward — is not wired in the
     dialer).
   - Render the "Via SSH bastion" `Select` inside the existing `isKubernetes`
     block, with `data-testid="k8s-server-edit-via-select"`, the same
     `"none"` sentinel the create dialog uses and the same helper text.
   - SSH rows get nothing: the field lives under `isKubernetes`, mirroring the
     create dialog's `!isSSH`.
2. **Submit diff** (Kubernetes branch only): when
   `viaUid !== (server.via_uid || "")`, send `via_uid: viaUid` if non-empty,
   else `clear_via_uid: true`; unchanged sends neither. The SSH branch is left
   alone.
3. **E2E** (`front/e2e/servers.spec.ts`): one test that creates a bastion and a
   cluster, opens the cluster's edit dialog, asserts the selector reads
   "Direct (no tunnel)", picks the bastion, saves, reopens and asserts the
   bastion is selected, then clears it back to direct, saves, reopens and
   asserts it reads direct again.
4. No backend change: `UpdateDatabaseRequest` already carries `ViaUID` /
   `ClearViaUID` (`internal/api/servers.go:80-83`), the handler threads them at
   `internal/api/servers.go:537-538`, and `store.validateViaUID`
   (`internal/store/servers.go:265`) explicitly allows a Kubernetes row to sit
   behind an SSH bastion. Verified by reading both before touching the form.
