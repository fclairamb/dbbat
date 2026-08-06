/**
 * The product visuals on the homepage.
 *
 * Every asset here comes out of `make showcase` (see `front/showcase/`), which
 * drives a real demo-mode dbbat instance. `manifest.json` is imported rather
 * than fetched so the "captured on …" caption is baked in at build time: the
 * media is regenerated on demand, not on release, and a visitor deserves to
 * know which version they are looking at.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";
import Heading from "@theme/Heading";
import manifest from "@site/static/img/showcase/manifest.json";

import styles from "./styles.module.css";

type Still = {
  src: string;
  alt: string;
  title: string;
  description: string;
};

const STILLS: Still[] = [
  {
    src: "/img/showcase/query-list.png",
    alt: "The Queries page listing five statements with user, database, connection, duration, row count and status",
    title: "Every query, tracked",
    description:
      "Full SQL text, the user behind it, the connection it ran on, its duration and its row count — for every statement that crosses the proxy.",
  },
  {
    src: "/img/showcase/query-results.png",
    alt: "A query detail page showing the SQL, its duration, its row count and the ten result rows it returned",
    title: "Down to the rows returned",
    description:
      "Open a statement to see what it actually did, optional captured result rows included.",
  },
  {
    src: "/img/showcase/grant-request.png",
    alt: "The Request access dialog with a grant definition, a database and a written justification",
    title: "Access on request",
    description:
      "A developer picks a grant definition and a database and says why. An admin approves or denies — from the UI or straight from Slack.",
  },
  {
    src: "/img/showcase/add-server.png",
    alt: "The Add Server dialog configuring a PostgreSQL read replica",
    title: "One proxy, four engines",
    description:
      "Point DBBat at PostgreSQL, Oracle, MySQL/MariaDB or MongoDB. Credentials are encrypted at rest; clients never see them.",
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

function ApprovalVideo(): ReactNode {
  const videoRef = useRef<HTMLVideoElement>(null);
  const preference = useMotionPreference();
  const animate = preference === "ok";

  // No `autoPlay` attribute anywhere — that is the point. The prerendered HTML
  // must not carry one, or a reduced-motion visitor gets a moving picture in
  // the window between paint and hydration (and gets one for good with JS
  // off). Playback is started here instead, only once the media query has
  // said it is welcome.
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
    video.muted = true;
    void video.play().catch(() => {
      // Autoplay refused (a battery-saver mode, a strict browser setting).
      // The controls are still there; nothing else to do.
    });
  }, [animate]);

  return (
    <figure className={styles.videoFigure}>
      <video
        ref={videoRef}
        className={styles.video}
        loop={animate}
        controls={!animate}
        muted
        playsInline
        preload="metadata"
        poster="/img/showcase/approval-hold-poster.png"
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
              <a href={still.src} target="_blank" rel="noopener noreferrer">
                <img
                  src={still.src}
                  alt={still.alt}
                  width={2560}
                  height={1600}
                  loading="lazy"
                  decoding="async"
                />
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
