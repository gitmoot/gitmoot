import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Gitmoot',
  tagline: 'Local-first multi-agent coordination for software projects',
  favicon: 'img/gitmoot-logo.svg',

  url: 'https://gitmoot.io',
  baseUrl: '/docs/',
  organizationName: 'jerryfane',
  projectName: 'gitmoot',

  onBrokenLinks: 'throw',
  trailingSlash: false,

  future: {
    v4: true,
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],
  plugins: [
    [
      '@docusaurus/plugin-client-redirects',
      {
        redirects: [
          {
            from: '/',
            to: '/intro',
          },
        ],
      },
    ],
  ],

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/jerryfane/gitmoot/tree/main/website/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/gitmoot-hero.png',
    colorMode: {
      defaultMode: 'light',
      respectPrefersColorScheme: false,
    },
    navbar: {
      title: 'Gitmoot',
      logo: {
        alt: 'Gitmoot',
        src: 'img/gitmoot-logo.svg',
        href: '/intro',
      },
      items: [
        {type: 'docSidebar', sidebarId: 'docsSidebar', position: 'left', label: 'Docs'},
        {to: '/getting-started/install', label: 'Install', position: 'left'},
        {to: '/workflows/pr-comment-workflow', label: 'Workflows', position: 'left'},
        {to: '/reference/cli', label: 'Reference', position: 'left'},
        {
          href: 'https://gitmoot.io',
          label: 'Website',
          position: 'right',
        },
        {
          href: 'https://github.com/jerryfane/gitmoot',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      logo: {
        alt: 'Gitmoot',
        src: 'img/gitmoot-logo.svg',
        href: '/intro',
        width: 40,
        height: 40,
      },
      links: [
        {
          title: 'Start',
          items: [
            {label: 'Introduction', to: '/intro'},
            {label: 'Install', to: '/getting-started/install'},
            {label: 'Quick Start', to: '/getting-started/quick-start'},
          ],
        },
        {
          title: 'Operate',
          items: [
            {label: 'PR Comments', to: '/workflows/pr-comment-workflow'},
            {label: 'Planner Goals', to: '/workflows/planner-goal-workflow'},
            {label: 'Troubleshooting', to: '/operations/troubleshooting'},
          ],
        },
        {
          title: 'Reference',
          items: [
            {label: 'CLI', to: '/reference/cli'},
            {label: 'Runtime Adapters', to: '/reference/runtime-adapters'},
            {label: 'SKILL.md', href: 'https://gitmoot.io/SKILL.md'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Gitmoot contributors.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;

