/**
 * The blueprint downloads of the docs site (issue 3137).
 *
 * At the end of `astro build` this writes the built blueprints into the site
 * under `blueprints/`: one importable file per agent and team blueprint, the
 * same bytes the release attaches and the web app's gallery serves, because
 * all three come from scripts/blueprints/build.mjs reading the repository's
 * blueprints/ tree. Nothing is copied by hand, and nothing is committed under
 * public/. The how-to links them as `/blueprints/agents/<slug>.agent.md` and
 * `/blueprints/teams/<slug>.team.zip` under the site's base.
 */
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import type { AstroIntegration } from 'astro';
import { parse, stringify } from 'yaml';

// A static import: the config's Vite module runner is closed by astro:build:done, so a dynamic import there fails.
import { buildBlueprints } from '../../../../scripts/blueprints/build.mjs';

const repositoryRoot = fileURLToPath(new URL('../../../../', import.meta.url));

export function blueprintDownloads(): AstroIntegration {
  return {
    name: 'elitea-blueprints',
    hooks: {
      'astro:build:done': async ({ dir, logger }) => {
        const { files } = await buildBlueprints({ root: repositoryRoot, parseYaml: parse, dumpYaml: stringify });
        const out = path.join(fileURLToPath(dir), 'blueprints');
        for (const [relative, data] of files) {
          if (relative === 'release-notes.md') continue;
          const target = path.join(out, relative);
          await mkdir(path.dirname(target), { recursive: true });
          await writeFile(target, data);
        }
        logger.info(`wrote ${String(files.size - 1)} blueprint files to blueprints/`);
      },
    },
  };
}
