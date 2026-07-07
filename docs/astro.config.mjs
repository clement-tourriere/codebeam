import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  // GitHub Pages project site: served under /codebeam/.
  site: 'https://clement-tourriere.github.io',
  base: '/codebeam',
  integrations: [
    starlight({
      title: 'Codebeam',
      description:
        'Self-hosted code search for you and your AI agents — one binary, always fresh, from laptop to team server.',
      social: [
        {
          icon: 'github',
          label: 'GitHub repository',
          href: 'https://github.com/clement-tourriere/codebeam',
        },
      ],
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'What is Codebeam?', slug: '' },
            { label: 'Get started', slug: 'getting-started' },
          ],
        },
        {
          label: 'Use Codebeam',
          items: [
            { label: 'Searching', slug: 'searching' },
            { label: 'The cb CLI', slug: 'cli' },
            { label: 'Repositories and indexing', slug: 'repositories-indexing' },
            { label: 'AI agents and APIs', slug: 'integrations' },
          ],
        },
        {
          label: 'Run an instance',
          items: [
            { label: 'Authentication and access', slug: 'oauth' },
            { label: 'Configuration', slug: 'configuration' },
            { label: 'Deployment', slug: 'deployment' },
            { label: 'Troubleshooting', slug: 'troubleshooting' },
          ],
        },
      ],
    }),
  ],
});
