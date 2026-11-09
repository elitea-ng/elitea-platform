import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';
import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';

export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
  // The upgrade notes: one file per change in the repository's
  // docs/upgrading/ (docs/upgrading/README.md), assembled newest first on
  // /deployment/upgrading by src/components/UpgradeNotes.astro. The id is the
  // file name without `.md`, which is also each entry's anchor.
  // scripts/ci/check-upgrading-fragments.mjs holds the front matter to this
  // shape before a build sees it.
  upgrading: defineCollection({
    loader: glob({
      base: '../../docs/upgrading',
      pattern: '[0-9]*.md',
      generateId: ({ entry }) => entry.replace(/\.md$/, ''),
    }),
    schema: z.object({
      title: z.string().min(1),
      issue: z.number().int().positive(),
      // YAML reads a bare YYYY-MM-DD as a date.
      date: z.coerce.date(),
      adr: z.string().regex(/^ADR-\d{4}$/).optional(),
      order: z.number().int().positive().optional(),
    }),
  }),
};
