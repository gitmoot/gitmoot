import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Gitmoot',
  tagline: 'Local-first multi-agent coordination for software projects',
  favicon: 'img/favicon.svg',

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
    image: 'https://gitmoot.io/gitmoot-og-image.png',
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: false,
    },
    navbar: {
      title: 'Gitmoot',
      logo: {
        alt: 'Gitmoot',
        src: 'img/gitmoot-char.svg',
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
        {
          href: 'https://discord.gg/TTFRHFyDXf',
          label: 'Discord',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      logo: {
        alt: 'Gitmoot',
        src: 'img/gitmoot-char.svg',
        href: '/intro',
        width: 28,
        height: 28,
      },
      links: [
        {label: 'Install script', href: 'https://gitmoot.io/install.sh'},
        {label: 'SKILL.md', href: 'https://gitmoot.io/SKILL.md'},
        {label: 'Website', href: 'https://gitmoot.io'},
        {label: 'GitHub', href: 'https://github.com/jerryfane/gitmoot'},
        {label: 'Discord', href: 'https://discord.gg/TTFRHFyDXf'},
      ],
      copyright: `© ${new Date().getFullYear()} Gitmoot`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
