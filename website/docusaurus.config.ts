// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Kairon',
  tagline: 'Real VMs on Kubernetes — without KubeVirt',
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
  },

  url: 'https://zyvorai.github.io',
  baseUrl: '/zyvor-kairon/',

  organizationName: 'zyvorai',
  projectName: 'zyvor-kairon',

  onBrokenLinks: 'warn',

  markdown: {
    format: 'md',
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  themes: ['@docusaurus/theme-mermaid'],

  staticDirectories: ['static', '../docs/ux'],

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs',
          routeBasePath: 'docs',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/zyvorai/zyvor-kairon/tree/main/docs/',
          exclude: ['**/assets/**'],
        },
        blog: false,
        theme: {
          customCss: ['./src/css/custom.css', './src/css/zv.css'],
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/social-preview.png',
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: false,
    },
    navbar: {
      title: 'Kairon',
      logo: {
        alt: 'Kairon',
        src: 'img/favicon.svg',
      },
      hideOnScroll: false,
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'right',
          label: 'Docs',
        },
        {to: '/deploy', label: 'Deploy', position: 'right'},
        {to: '/gallery', label: 'Gallery', position: 'right'},
        {
          href: 'https://github.com/zyvorai/zyvor-kairon',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Deploy', to: '/deploy'},
            {label: 'Getting started', to: '/docs/getting-started'},
            {label: 'What ships', to: '/docs/WHAT_SHIPS'},
            {label: 'Status', to: '/docs/STATUS'},
            {label: 'CLI', to: '/docs/CLI'},
            {label: 'Architecture', to: '/docs/architecture'},
            {label: 'Network Fabric', to: '/docs/network-fabric'},
          ],
        },
        {
          title: 'Project',
          items: [
            {label: 'GitHub', href: 'https://github.com/zyvorai/zyvor-kairon'},
            {label: 'Roadmap', href: 'https://github.com/zyvorai/zyvor-kairon/blob/main/ROADMAP.md'},
            {label: 'License (Apache-2.0)', href: 'https://github.com/zyvorai/zyvor-kairon/blob/main/LICENSE'},
          ],
        },
        {
          title: 'Zyvor Enterprise',
          items: [
            {label: 'zyvor.dev', href: 'https://zyvor.dev'},
            {label: 'sales@zyvor.dev', href: 'mailto:sales@zyvor.dev'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Zyvor. Kairon core is Apache-2.0 licensed.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
