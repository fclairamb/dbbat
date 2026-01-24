import { themes as prismThemes } from "prism-react-renderer";
import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";

const config: Config = {
  title: "DBBat",
  tagline:
    "Give (temporary) accesses to prod databases. Every query logged. Every data saved.",
  favicon: "img/favicon.ico",

  url: "https://dbbat.com",
  baseUrl: "/",

  organizationName: "fclairamb",
  projectName: "dbbat",
  trailingSlash: false,

  onBrokenLinks: "throw",
  onBrokenMarkdownLinks: "warn",

  future: {
    v4: true,
  },

  i18n: {
    defaultLocale: "en",
    locales: ["en"],
  },

  presets: [
    [
      "classic",
      {
        docs: {
          sidebarPath: "./sidebars.ts",
          editUrl: "https://github.com/fclairamb/dbbat/tree/main/website/",
        },
        blog: false,
        theme: {
          customCss: "./src/css/custom.css",
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    navbar: {
      title: "DBBat",
      logo: {
        alt: "DBBat Logo",
        src: "img/logo-notext.png",
      },
      items: [
        {
          type: "docSidebar",
          sidebarId: "tutorialSidebar",
          position: "left",
          label: "Documentation",
        },
        {
          to: "/docs/changelog",
          label: "Changelog",
          position: "left",
        },
        {
          href: "https://demo.dbbat.com",
          label: "Demo",
          position: "right",
        },
        {
          href: "https://github.com/fclairamb/dbbat",
          label: "GitHub",
          position: "right",
        },
      ],
    },
    footer: {
      style: "dark",
      links: [
        {
          title: "Docs",
          items: [
            {
              label: "Getting Started",
              to: "/docs/intro",
            },
            {
              label: "Installation",
              to: "/docs/installation/docker",
            },
            {
              label: "Configuration",
              to: "/docs/configuration",
            },
          ],
        },
        {
          title: "Community",
          items: [
            {
              label: "GitHub Issues",
              href: "https://github.com/fclairamb/dbbat/issues",
            },
            {
              label: "GitHub Discussions",
              href: "https://github.com/fclairamb/dbbat/discussions",
            },
          ],
        },
        {
          title: "Resources",
          items: [
            {
              label: "Changelog",
              to: "/docs/changelog",
            },
            {
              label: "Live Demo",
              href: "https://demo.dbbat.com",
            },
          ],
        },
        {
          title: "More",
          items: [
            {
              label: "GitHub",
              href: "https://github.com/fclairamb/dbbat",
            },
          ],
        },
      ],
      copyright: `&copy; DBBat ${new Date().getFullYear()}`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ["bash", "yaml", "docker", "json", "go", "sql"],
    },
    colorMode: {
      defaultMode: "light",
      disableSwitch: false,
      respectPrefersColorScheme: true,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
