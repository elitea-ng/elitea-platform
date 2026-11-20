import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';
import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';

export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
  // The entries of the What's new page, one file per change, so open pull
  // requests never conflict over one shared page (src/whats-new.ts renders
  // them; scripts/ci/check-whats-new.mjs holds the rules).
  whatsNew: defineCollection({
    loader: glob({ pattern: '*.md', base: './src/content/whats-new' }),
    schema: z.strictObject({ date: z.coerce.date(), order: z.number().int().default(0) }),
  }),
};
