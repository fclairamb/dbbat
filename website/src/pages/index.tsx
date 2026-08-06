import type { ReactNode } from "react";
import clsx from "clsx";
import Link from "@docusaurus/Link";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import Layout from "@theme/Layout";
import HomepageFeatures from "@site/src/components/HomepageFeatures";
import ProductShowcase from "@site/src/components/ProductShowcase";
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

function QuickStart() {
  return (
    <section className={styles.quickStart}>
      <div className="container">
        <Heading as="h2">Quick Start</Heading>
        <p>
          Get DBBat running in seconds with Docker — one container fronts
          PostgreSQL, Oracle, MySQL/MariaDB, and MongoDB:
        </p>
        <pre className={styles.codeBlock}>
          <code>
            docker run
            <br />
            &nbsp;&nbsp;-p 5433:5433&nbsp;&nbsp;# PostgreSQL proxy
            <br />
            &nbsp;&nbsp;-p 1522:1522&nbsp;&nbsp;# Oracle proxy
            <br />
            &nbsp;&nbsp;-p 3307:3307&nbsp;&nbsp;# MySQL / MariaDB proxy
            <br />
            &nbsp;&nbsp;-p 27018:27018&nbsp;&nbsp;# MongoDB proxy
            <br />
            &nbsp;&nbsp;-p 4200:4200&nbsp;&nbsp;# REST API + web UI
            <br />
            &nbsp;&nbsp;-e
            DBB_DSN=postgres://dbbat:dbbat@pgserver:5432/dbbat
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

export default function Home(): ReactNode {
  const { siteConfig } = useDocusaurusContext();
  return (
    <Layout
      title={`${siteConfig.title} - Database Observability Proxy`}
      description="Give your devs (temporary) access to prod. PostgreSQL, Oracle, MySQL/MariaDB, and MongoDB proxy with full query logging, fine-grained access control, and session capture."
    >
      <HomepageHeader />
      <main>
        <HomepageFeatures />
        <ProductShowcase />
        <QuickStart />
      </main>
    </Layout>
  );
}
