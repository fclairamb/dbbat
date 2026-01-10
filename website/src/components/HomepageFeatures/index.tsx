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
    title: "Transparent Proxy",
    emoji: "🔌",
    description: (
      <>
        Connect any PostgreSQL client through DBBat without code changes.
        Standard PostgreSQL wire protocol support means zero application
        modifications required.
      </>
    ),
  },
  {
    title: "Query Observability",
    emoji: "🔍",
    description: (
      <>
        Track every query with full SQL text, execution time, rows affected, and
        optional result data capture. Gain complete visibility into database
        activity.
      </>
    ),
  },
  {
    title: "Access Control",
    emoji: "🔐",
    description: (
      <>
        Grant time-windowed access with read/write permissions. Set quotas on
        queries and data transfer. Automatically expire or manually revoke
        access.
      </>
    ),
  },
  {
    title: "User Management",
    emoji: "👥",
    description: (
      <>
        Create users with their own credentials separate from database
        credentials. Admin users can manage all resources and grants.
      </>
    ),
  },
  {
    title: "Secure by Design",
    emoji: "🛡️",
    description: (
      <>
        Passwords hashed with Argon2id, database credentials encrypted with
        AES-256-GCM. Full audit logging of all access control changes.
      </>
    ),
  },
  {
    title: "REST API",
    emoji: "🌐",
    description: (
      <>
        Full-featured REST API with OpenAPI documentation. Manage users,
        databases, grants, and view connections and queries programmatically.
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
