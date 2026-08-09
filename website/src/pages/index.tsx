import type { ReactNode } from "react";
import clsx from "clsx";
import Head from "@docusaurus/Head";
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
      {/* React only auto-preloads a bare <img>; wrapping the hero logo in a
          <picture> makes it (rightly) give up, since it cannot know which
          <source> will win. So we preload the WebP by hand — `type` gates the
          hint on decoder support, so a browser that would fall back to the PNG
          simply ignores it rather than fetching both. */}
      <Head>
        <link
          rel="preload"
          as="image"
          href="/img/logo-text.webp"
          type="image/webp"
        />
      </Head>
      <div className="container">
        {/* Above the fold on every cold visit, so it is served at the size it
            actually renders (600px for a 300 CSS px box at DPR 2) rather than
            at its 761px source resolution. WebP for anything that can decode
            it, the PNG as the fallback — see `website/img-src/README.md` for
            the originals and the encoder invocations. */}
        <picture>
          <source srcSet="/img/logo-text.webp" type="image/webp" />
          <img
            src="/img/logo-text.png"
            alt={siteConfig.title}
            className={styles.heroLogo}
            width={600}
            height={600}
            fetchPriority="high"
          />
        </picture>
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
          PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, and SQL Server:
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
            &nbsp;&nbsp;-p 1434:1434&nbsp;&nbsp;# SQL Server proxy
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
      description="Give your devs and AI agents (temporary) access to prod. PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, and SQL Server proxy with full query logging, fine-grained access control, and session capture."
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
