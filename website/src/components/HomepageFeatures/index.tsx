import clsx from "clsx";
import Heading from "@theme/Heading";
import styles from "./styles.module.css";

type FeatureItem = {
  title: string;
  emoji: string;
  description: JSX.Element;
};

const FeatureList: FeatureItem[] = [
  {
    title: "Multi-Engine Proxy",
    emoji: "🔌",
    description: (
      <>
        PostgreSQL, Oracle, and MySQL/MariaDB on independent listeners. Connect
        any standard client — psql, sqlplus, mysql, DBeaver, your ORM — without
        application changes.
      </>
    ),
  },
  {
    title: "Query Observability",
    emoji: "🔍",
    description: (
      <>
        Track every query with full SQL text, parameters, execution time, rows
        affected, and optional result-data capture. Text and binary protocols
        decoded the same way.
      </>
    ),
  },
  {
    title: "Granular Access Control",
    emoji: "🔐",
    description: (
      <>
        Time-windowed grants with combinable controls — <code>read_only</code>,{" "}
        <code>block_copy</code>, <code>block_ddl</code> — plus per-grant quotas
        on queries and bytes transferred.
      </>
    ),
  },
  {
    title: "User & API Key Management",
    emoji: "👥",
    description: (
      <>
        Local users with <code>admin</code>, <code>viewer</code>, and{" "}
        <code>connector</code> roles. Optional Slack sign-in. API keys for
        programmatic access — and they cannot create or revoke other keys.
      </>
    ),
  },
  {
    title: "Secure by Design",
    emoji: "🛡️",
    description: (
      <>
        Passwords hashed with Argon2id, database credentials encrypted with
        AES-256-GCM and bound to the database UID. Append-only audit log of
        every administrative change.
      </>
    ),
  },
  {
    title: "Defense in Depth",
    emoji: "🧱",
    description: (
      <>
        Read-only enforced via SQL inspection <em>and</em> engine-level session
        flags. MySQL <code>LOCAL INFILE</code> opted out of the upstream
        capabilities. PostgreSQL session bypass attempts blocked.
      </>
    ),
  },
  {
    title: "Session Packet Dumps",
    emoji: "📼",
    description: (
      <>
        Optional per-session binary capture of the post-auth command stream.
        Same <code>.dbbat-dump</code> format across all protocols, with a CLI
        anonymiser for safe sharing.
      </>
    ),
  },
  {
    title: "REST API + Web UI",
    emoji: "🌐",
    description: (
      <>
        Full OpenAPI 3.0 spec served at <code>/api/docs</code>. React frontend
        embedded in the binary at <code>/app</code> for managing users,
        databases, grants, and browsing query history.
      </>
    ),
  },
  {
    title: "Single Binary",
    emoji: "📦",
    description: (
      <>
        One Go binary backed by PostgreSQL. Distroless Docker image, Helm chart,
        Kubernetes-friendly. Stateless beyond its store — replicas welcome.
      </>
    ),
  },
];

function Feature({ title, emoji, description }: FeatureItem) {
  return (
    <div className={clsx("col col--4")}>
      <div className="text--center">
        <span className={styles.featureEmoji}>{emoji}</span>
      </div>
      <div className="text--center padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): JSX.Element {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
