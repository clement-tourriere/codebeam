import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  integrations: [
    starlight({
      title: 'Codebeam',
      description: 'Setup and operations documentation for Codebeam instances.',
      social: [
        {
          icon: 'github',
          label: 'GitHub repository',
          href: 'https://github.com/ctourriere/codebeam',
        },
      ],
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'Overview', slug: '' },
            { label: 'Getting started', slug: 'getting-started' },
            { label: 'Configuration', slug: 'configuration' },
          ],
        },
        {
          label: 'Instance setup',
          items: [
            { label: 'OAuth and tokens', slug: 'oauth' },
            { label: 'Repositories and indexing', slug: 'repositories-indexing' },
            { label: 'Deployment', slug: 'deployment' },
          ],
        },
        {
          label: 'Use Codebeam',
          items: [
            { label: 'Integrations and APIs', slug: 'integrations' },
            { label: 'Troubleshooting', slug: 'troubleshooting' },
          ],
        },
      ],
    }),
  ],
});
