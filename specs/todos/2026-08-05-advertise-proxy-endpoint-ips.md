# Advertise the proxy endpoint's IP addresses, not just its hostname

## Goal

Show the IP addresses behind the advertised connection host next to the
connection string — in the database Connect panel and in Settings' public
endpoint advertisement section — with a one-line note that VPN clients need
every one of them.

## Why

A user who cannot reach the proxy is told, by us and by every wrapper around us,
to "connect to `db.example.com`" — a name. VPN clients route addresses:
WireGuard's `AllowedIPs`, an IPsec split-tunnel policy, a firewall rule all take
IPs or CIDRs. So the connection string we hand out is not enough to open the
tunnel that makes it usable, and the missing piece has to travel out of band —
over chat, from someone who happens to know.

It also gets answered *wrongly* out of band. A load balancer spanning several
availability zones has one address per zone, and DNS publishes only the zone
currently serving traffic; `dig` therefore returns today's IP, and a VPN config
built from it breaks at the next pod reschedule. Anyone answering the question
by resolving the name gives a one-IP answer that looks right and expires.

dbbat already knows the host it advertises, so it is the natural place to resolve
it and show the full picture. Documented generically in
`website/docs/installation/kubernetes.md` ("Client-side reachability"); this todo
is the product-side half.

## Implementation

- `internal/api/` — resolve the advertised connection hosts (`ResolvedEndpoints`
  in `internal/api/connection_url.go` already computes the host per protocol)
  and expose the addresses, e.g. a `resolved_ips` field alongside the existing
  `resolved` block of `GET /api/v1/settings` and/or on
  `GET /api/v1/servers/{uid}/connection`. Cache with a short TTL — this is a DNS
  lookup on a request path — and never fail the response on a lookup error: the
  field is additive, an empty list is a fine answer.
- Resolution only sees what DNS publishes, which is the trap above. Do not
  present the result as "the IPs to allow" — present it as "resolves to, right
  now", and let the operator record the authoritative per-zone set in a
  free-text field (a `connection_note` on the settings, rendered under the
  connection string) that the deployment owns.
- `front/src/routes/_authenticated/settings/index.tsx` — display it in the
  public endpoint advertisement card, where `host` / `pg_host` / … are already
  shown.
- Connect panel: one muted line under the copyable URL, so the person about to
  fail at connecting sees it before they file the ticket.

No GitHub issue filed yet — one should be opened.
