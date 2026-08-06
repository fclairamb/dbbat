# Make the approval poster reproducible

## Goal

Bring `approval-hold-poster.png` (and its WebP sibling) under the same
byte-identity guarantee the other four stills now have, so a showcase
regeneration stops carrying an unreviewable ~120 KB binary diff.

## Why

`specs/done/2026/08/2026-08-06-showcase-reproducible-own-rows.md` normalised the
observability timeline, and two consecutive pipeline runs against the same
binary now emit byte-identical `add-server`, `grant-request`, `query-list` and
`query-results`. The poster is the last one out:

- it is a frame of the **video** project, which deliberately does not pin the
  clock (`shouldFreezeClock("video")` is false) — a frozen `Date.now()` renders
  the "held for 3s" counter on a live hold as a constant, and can render it
  negative for a statement that arrived after the pin;
- the frame it captures shows a grant validity window at the real clock
  ("Aug 6, 2026 at 14:17 to Aug 6, 2026 at 22:18") — `access_grants` is
  deliberately left alone by `lib/normalise.ts`, because the video's proxy
  session has to authenticate through that grant while it is genuinely valid;
- the held statement's own elapsed counter ticks in wall time.

So every regeneration rewrites it, and the diff cannot be reviewed.

## Implementation

The poster is a *still*, unlike the clip around it — it is what the site shows
under `prefers-reduced-motion`. That is the opening: it does not have to be a
frame of the recording.

Sketch: capture it in the `screenshots` project instead, as its own scenario —
park an `UPDATE` on a hold exactly as `approval-video.spec.ts` does, then pin
the clock to the same constant the other stills use and normalise the hold's
own timestamps the way `lib/normalise.ts` normalises the query timeline. The
grant is the sticking point: it must stay valid at the real clock for the proxy
to accept the session, but its rendered validity window is what churns. Options
worth weighing:

- normalise `access_grants.starts_at` / `expires_at` *after* the hold is parked
  and the frame is taken, but before the video project runs — ordering is
  already serial and single-worker;
- or capture the poster from a scrolled position that excludes the Grant card,
  which is cheaper but loses context the frame currently carries;
- or give the approval registry a fixed "held for" value under a capture flag.

Key files: `front/showcase/approval-video.spec.ts`,
`front/showcase/screenshots.spec.ts`, `front/showcase/lib/normalise.ts`,
`front/showcase/config.ts` (`shouldFreezeClock`), `scripts/showcase.sh`.

Note the clip itself (`approval-hold-av1.mp4` / `-h264.mp4`) is out of scope: a
recording of a live session re-encoded by ffmpeg is not going to be
byte-stable, and nobody reviews it as a diff.

No GitHub issue yet — one should be filed.
