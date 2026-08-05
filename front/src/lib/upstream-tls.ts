/**
 * Classification of `connections.upstream_tls` — whether the proxy→database
 * leg of one session was actually encrypted.
 *
 * The column records an *outcome*, not a policy: the opportunistic ssl_mode
 * values (`prefer`, and the empty default every unset server row carries)
 * offer TLS and fall back to plaintext when the target refuses, so only the
 * session itself knows which way it went. A silent downgrade is exactly the
 * case an operator needs to notice, which is why `plaintext` is the only
 * state the UI renders as a badge — everything else stays deliberately quiet.
 */
export type UpstreamTlsState =
  | "encrypted"
  | "plaintext"
  // Same raw `false`, but we can't attribute it: the caller doesn't know the
  // protocol (a non-admin only ever sees uid/name/description for a server),
  // and Oracle is always false by design. Reported honestly, without the
  // warning styling we'd otherwise be putting on every Oracle row.
  | "plaintext-unattributed"
  // Oracle: the proxy relays the client's own TNS Connect descriptor over a
  // plain socket and never upgrades that leg. `false` is structural here, not
  // a downgrade, so it must not read as a warning.
  | "not-applicable"
  | "unknown";

/** Classify a session, given the protocol of the server it targeted. */
export function upstreamTlsState(
  upstreamTls: boolean | undefined,
  protocol: string | undefined,
): UpstreamTlsState {
  if (upstreamTls === undefined) {
    return "unknown";
  }
  if (upstreamTls) {
    return "encrypted";
  }
  if (protocol === "oracle") {
    return "not-applicable";
  }
  if (!protocol) {
    return "plaintext-unattributed";
  }
  return "plaintext";
}

/** Short label shown in the connections table and detail page. */
export const UPSTREAM_TLS_LABELS: Record<UpstreamTlsState, string> = {
  encrypted: "TLS",
  plaintext: "Plaintext",
  "plaintext-unattributed": "Plaintext",
  "not-applicable": "n/a",
  unknown: "-",
};

/**
 * One sentence an operator can act on. `sslMode` is the server's configured
 * policy (admin-visible only); pairing it with the outcome is what turns two
 * separate facts into the single readable statement "policy said prefer,
 * outcome was plaintext".
 */
export function upstreamTlsExplanation(
  state: UpstreamTlsState,
  sslMode?: string,
): string {
  const policy = sslMode ? ` The server's ssl_mode policy is "${sslMode}".` : "";
  switch (state) {
    case "encrypted":
      return `The proxy→database leg of this session was encrypted with TLS.${policy}`;
    case "plaintext":
      return (
        "The proxy→database leg of this session ran in the clear — no TLS." +
        policy +
        " Opportunistic modes fall back to plaintext when the target refuses TLS."
      );
    case "plaintext-unattributed":
      return (
        "The proxy→database leg of this session was not encrypted. The target's " +
        "protocol isn't visible to your role, and Oracle sessions are always " +
        "plaintext by design, so this may be expected."
      );
    case "not-applicable":
      return (
        "Not applicable for Oracle: the proxy relays the client's own TNS " +
        "Connect descriptor over a plain socket and never upgrades that leg."
      );
    case "unknown":
      return "This session predates upstream-TLS recording.";
  }
}
