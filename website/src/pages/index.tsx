import clsx from "clsx";
import Link from "@docusaurus/Link";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import Layout from "@theme/Layout";
import HomepageFeatures from "@site/src/components/HomepageFeatures";
import Heading from "@theme/Heading";

import styles from "./index.module.css";

function HomepageHeader() {
  const { siteConfig } = useDocusaurusContext();
  return (
    <header className={clsx("hero hero--primary", styles.heroBanner)}>
      <div className="container">
        <img
          src="/img/logo-text.png"
          alt={siteConfig.title}
          className={styles.heroLogo}
        />
        <p className="hero__subtitle">{siteConfig.tagline}</p>
        <div className={styles.buttons}>
          <Link
            className="button button--secondary button--lg"
            to="/docs/intro"
          >
            Get Started
          </Link>
          <Link
            className="button button--secondary button--lg"
            href="https://demo.dbbat.com"
          >
            Try Demo
          </Link>
          <Link
            className="button button--secondary button--lg"
            href="https://github.com/fclairamb/dbbat"
          >
            View on GitHub
          </Link>
        </div>
        <p className={styles.demoCredentials}>
          Demo login: <code>admin</code> / <code>admin</code>
        </p>
      </div>
    </header>
  );
}

const screenshots = [
  {
    src: "/img/screenshots/screenshot-dashboard.png",
    alt: "Dashboard showing recent connections and activity",
    caption: "Dashboard",
  },
  {
    src: "/img/screenshots/screenshot-queries.png",
    alt: "Query log with SQL details and execution times",
    caption: "Query Logging",
  },
  {
    src: "/img/screenshots/screenshot-grants.png",
    alt: "Grant management with time-based access controls",
    caption: "Access Control",
  },
];

function Screenshots() {
  return (
    <section className={styles.screenshots}>
      <div className="container">
        <Heading as="h2">See it in Action</Heading>
        <p>
          Explore the DBBat interface for managing database access and monitoring
          queries.
        </p>
        <div className={styles.screenshotsGrid}>
          {screenshots.map((screenshot) => (
            <figure key={screenshot.src} className={styles.screenshotCard}>
              <a href={screenshot.src} target="_blank" rel="noopener noreferrer">
                <img src={screenshot.src} alt={screenshot.alt} loading="lazy" />
              </a>
              <figcaption>{screenshot.caption}</figcaption>
            </figure>
          ))}
        </div>
      </div>
    </section>
  );
}

function QuickStart() {
  return (
    <section className={styles.quickStart}>
      <div className="container">
        <Heading as="h2">Quick Start</Heading>
        <p>Get DBBat running in seconds with Docker:</p>
        <pre className={styles.codeBlock}>
          <code>
            docker run
            <br />
            &nbsp;&nbsp;-p 5432:5432
            <br />
            &nbsp;&nbsp;-p 8080:8080
            <br />
            &nbsp;&nbsp;-e DBB_DSN=postgres://dbbat:dbbat@pgserver:5432/dbbat
            <br />
            &nbsp;&nbsp;ghcr.io/fclairamb/dbbat
          </code>
        </pre>
        <p>
          <Link to="/docs/installation/docker">
            View full installation guide
          </Link>
        </p>
      </div>
    </section>
  );
}

export default function Home(): JSX.Element {
  const { siteConfig } = useDocusaurusContext();
  return (
    <Layout
      title={`${siteConfig.title} - PostgreSQL Observability Proxy`}
      description="Give your devs access to prod. A PostgreSQL proxy with full query logging and access control."
    >
      <HomepageHeader />
      <main>
        <HomepageFeatures />
        <Screenshots />
        <QuickStart />
      </main>
    </Layout>
  );
}
