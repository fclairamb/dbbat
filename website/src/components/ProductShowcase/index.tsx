/**
 * The product visuals on the homepage.
 *
 * Every asset here comes out of `make showcase` (see `front/showcase/`), which
 * drives a real demo-mode dbbat instance. `manifest.json` is imported rather
 * than fetched so the "captured on …" caption is baked in at build time: the
 * media is regenerated on demand, not on release, and a visitor deserves to
 * know which version they are looking at.
 */
import {
  useEffect,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import Heading from "@theme/Heading";
import manifest from "@site/static/img/showcase/manifest.json";

import styles from "./styles.module.css";

type Still = {
  /** The full-size PNG: the <img> fallback, and what the link opens. */
  src: string;
  /** The 1280-wide WebP the grid actually shows — ~7% of the PNG's bytes. */
  webp: string;
  alt: string;
  title: string;
  description: string;
};

const STILLS: Still[] = [
  {
    src: "/img/showcase/query-list.png",
    webp: "/img/showcase/query-list.webp",
    alt: "The Queries page listing five statements with user, database, connection, duration, row count and status",
    title: "Every query, tracked",
    description:
      "Full SQL text, the user behind it, the connection it ran on, its duration and its row count — for every statement that crosses the proxy.",
  },
  {
    src: "/img/showcase/query-results.png",
    webp: "/img/showcase/query-results.webp",
    alt: "A query detail page showing the SQL, its duration, its row count and the ten result rows it returned",
    title: "Down to the rows returned",
    description:
      "Open a statement to see what it actually did, optional captured result rows included.",
  },
  {
    src: "/img/showcase/grant-request.png",
    webp: "/img/showcase/grant-request.webp",
    alt: "The Request access dialog with a grant definition, a database and a written justification",
    title: "Access on request",
    description:
      "A developer picks a grant definition and a database and says why. An admin approves or denies — from the UI or straight from Slack.",
  },
  {
    src: "/img/showcase/add-server.png",
    webp: "/img/showcase/add-server.webp",
    alt: "The Add Server dialog configuring a PostgreSQL read replica",
    title: "One proxy, five engines",
    description:
      "Point DBBat at PostgreSQL, Oracle, MySQL/MariaDB, MongoDB or SQL Server. Credentials are encrypted at rest; clients never see them.",
  },
];

/** en-GB + explicit UTC so the server and the browser render the same string. */
const CAPTURED_ON = new Intl.DateTimeFormat("en-GB", {
  dateStyle: "long",
  timeZone: "UTC",
}).format(new Date(manifest.generatedAt));

/**
 * Whether the visitor asked for reduced motion.
 *
 * Three states on purpose. Docusaurus prerenders this page in Node, where
 * `matchMedia` does not exist, so the first render cannot know — and the safe
 * assumption for a render that cannot know is "do not move". "unknown" is what
 * the server emits and what hydration starts from; the effect resolves it.
 */
type MotionPreference = "unknown" | "reduce" | "ok";

function useMotionPreference(): MotionPreference {
  const [preference, setPreference] = useState<MotionPreference>("unknown");

  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)");
    const sync = () => setPreference(query.matches ? "reduce" : "ok");
    sync();
    query.addEventListener("change", sync);
    return () => query.removeEventListener("change", sync);
  }, []);

  return preference;
}

/**
 * Whether the element has ever been near the viewport.
 *
 * The showcase sits well below the fold, and `play()` is what actually pulls
 * the ~330 KB video rendition down the wire. Gating it on visibility means a
 * visitor who never scrolls past the hero pays for the poster and nothing
 * else. Latching (rather than tracking) is deliberate: once it has started,
 * scrolling away should not stop it.
 */
function useHasBeenSeen(ref: RefObject<Element | null>): boolean {
  const [seen, setSeen] = useState(false);

  useEffect(() => {
    const element = ref.current;
    if (!element || seen) {
      return;
    }
    if (typeof IntersectionObserver === "undefined") {
      setSeen(true);
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setSeen(true);
          observer.disconnect();
        }
      },
      // A screen's worth of margin: start fetching just before it matters.
      { rootMargin: "200px" },
    );
    observer.observe(element);
    return () => observer.disconnect();
  }, [ref, seen]);

  return seen;
}

function ApprovalVideo(): ReactNode {
  const videoRef = useRef<HTMLVideoElement>(null);
  const preference = useMotionPreference();
  const animate = preference === "ok";
  const seen = useHasBeenSeen(videoRef);

  // No `autoPlay` attribute anywhere — that is the point. The prerendered HTML
  // must not carry one, or a reduced-motion visitor gets a moving picture in
  // the window between paint and hydration (and gets one for good with JS
  // off). Playback is started here instead, only once the media query has
  // said it is welcome *and* the clip is on screen.
  useEffect(() => {
    const video = videoRef.current;
    if (!video) {
      return;
    }
    if (!animate) {
      video.pause();
      video.currentTime = 0;
      return;
    }
    if (!seen) {
      return;
    }
    video.muted = true;
    void video.play().catch(() => {
      // Autoplay refused (a battery-saver mode, a strict browser setting).
      // The controls are still there; nothing else to do.
    });
  }, [animate, seen]);

  // `preload="none"`: nothing but the poster is fetched until the observer
  // above lets `play()` through, which is the whole point of gating it. The
  // intrinsic width/height reserve the frame in the meantime — with no
  // metadata to infer it from, a video defaults to 300x150.
  //
  // The poster is WebP with no markup-level fallback — a `poster=` attribute
  // cannot come from a <picture>. It is a still frame of the clip below, so
  // anything that can decode either <source> can decode it too, and the
  // figure's own background covers the frame for anything that cannot.
  return (
    <figure className={styles.videoFigure}>
      <video
        ref={videoRef}
        className={styles.video}
        loop={animate}
        controls={!animate}
        muted
        width={1280}
        height={800}
        preload="none"
        playsInline
        poster="/img/showcase/approval-hold-poster.webp"
        aria-label="An UPDATE statement is held mid-flight until a second person approves it in the DBBat UI"
      >
        <source
          src="/img/showcase/approval-hold-av1.mp4"
          type="video/mp4; codecs=av01.0.05M.08"
        />
        <source
          src="/img/showcase/approval-hold-h264.mp4"
          type="video/mp4; codecs=avc1.4d002a"
        />
        {/* Text, not an <img>: browsers fetch fallback content inside <video>
            even when they never render it, and the poster already covers the
            "cannot decode either rendition" case. */}
        This browser cannot play the clip — the screenshots below show the same
        UI.
      </video>
      <figcaption className={styles.videoCaption}>
        <strong>Four eyes on a write.</strong> An <code>UPDATE</code> matching
        the grant&apos;s approval pattern is parked mid-flight — the client sits
        there waiting — until a second person releases it. Self-approval is
        rejected.
        {preference === "reduce" ? (
          <span className={styles.motionNote}>
            {" "}
            Autoplay is off because your system asks for reduced motion — press
            play to watch it.
          </span>
        ) : null}
      </figcaption>
    </figure>
  );
}

export default function ProductShowcase(): ReactNode {
  return (
    <section className={styles.showcase}>
      <div className="container">
        <Heading as="h2">See it in action</Heading>
        <p className={styles.lead}>
          Real captures from a live instance — the query log, the access
          requests, and a write held for a second pair of eyes.
        </p>

        <ApprovalVideo />

        <div className={styles.grid}>
          {STILLS.map((still) => (
            <figure key={still.src} className={styles.card}>
              {/* The link opens the full-size PNG; the page itself loads the
                  1280-wide WebP, which is what the ~350 CSS px card needs. The
                  <img> keeps the PNG as the fallback for anything that cannot
                  decode WebP, and its intrinsic size is only there to reserve
                  the right aspect ratio. */}
              <a href={still.src} target="_blank" rel="noopener noreferrer">
                <picture>
                  <source srcSet={still.webp} type="image/webp" />
                  <img
                    src={still.src}
                    alt={still.alt}
                    width={2560}
                    height={1600}
                    loading="lazy"
                    decoding="async"
                  />
                </picture>
              </a>
              <figcaption>
                <strong>{still.title}</strong>
                <span>{still.description}</span>
              </figcaption>
            </figure>
          ))}
        </div>

        <p className={styles.provenance}>
          Captured on v{manifest.version} · {CAPTURED_ON}. The media is
          regenerated on demand, so a newer release may look a little different.
        </p>
      </div>
    </section>
  );
}
