/**
 * The hats a viewer can wear over a pending decision, as the API reports them
 * in `approver_role` on a grant request or a held query.
 *
 * An `approver_role` the map has no entry for — the empty string — means "you
 * may see this but not decide it": most often your own request or your own
 * held statement, neither of which anyone may resolve for themselves.
 *
 * Labels live here rather than next to either screen because both render the
 * same vocabulary, and the whole point of showing a hat is that it means the
 * same thing wherever it appears.
 */
export const APPROVER_HAT: Record<string, { label: string; hint: string }> = {
  admin: {
    label: "admin",
    hint: "You are an admin, so you can decide anything — except your own requests and your own held statements.",
  },
  definition_approver: {
    label: "definition approver",
    hint: "You are in one of the approver groups named on the grant definition this session's grant came from. That list wins over the server's own approvers.",
  },
  server_approver: {
    label: "server approver",
    hint: "You are in one of the approver groups resolved for the target server — its own list, or the union of the lists on the server groups it belongs to.",
  },
};
