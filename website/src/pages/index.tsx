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
            href="https://github.com/fclairamb/dbbat"
          >
            View on GitHub
          </Link>
        </div>
      </div>
    </header>
  );
}

function QuickStart() {
  return (
    <section className={styles.quickStart}>
      <div className="container">
        <Heading as="h2">Quick Start</Heading>
        <p>Get DBBat running in seconds with Docker:</p>
        <pre className={styles.codeBlock}>
          <code>docker run -p 5432:5432 -p 8080:8080 fclairamb/dbbat</code>
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
        <QuickStart />
      </main>
    </Layout>
  );
}
