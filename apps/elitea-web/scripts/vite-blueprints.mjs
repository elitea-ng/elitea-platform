/**
 * The blueprint gallery's files (issue 3137): `vite.config.ts`'s
 * `elitea-blueprints` plugin. The repo-root `scripts/blueprints/build.mjs`
 * turns `blueprints/` into importable files (index.json, agents/<slug>.agent.md
 * or .agent.zip, teams/<slug>.team.zip, avatars/…). The main app's build emits
 * them under `blueprints/` beside index.html (the desktop build too), and the
 * dev server serves them from memory, so `widgets/blueprint-gallery` fetches
 * `${BASE_URL}blueprints/index.json` the same way everywhere.
 *
 * The builder is injected for the test; the default loads the real one with
 * the web app's js-yaml. The build runs once per plugin instance.
 */
import { fileURLToPath } from 'node:url';

export const BLUEPRINTS_DIR = 'blueprints';

const REPO_ROOT = fileURLToPath(new URL('../../../', import.meta.url));

const CONTENT_TYPES = {
  json: 'application/json',
  md: 'text/markdown; charset=utf-8',
  zip: 'application/zip',
  png: 'image/png',
  webp: 'image/webp',
  gif: 'image/gif',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  svg: 'image/svg+xml',
  webm: 'video/webm',
  mp4: 'video/mp4',
};

/** The Content-Type of a built file, by extension. */
export function blueprintContentType(path) {
  const extension = path.slice(path.lastIndexOf('.') + 1).toLowerCase();
  return CONTENT_TYPES[extension] ?? 'application/octet-stream';
}

async function defaultBuild(root) {
  const [{ buildBlueprints }, yaml] = await Promise.all([
    import('../../../scripts/blueprints/build.mjs'),
    import('js-yaml'),
  ]);
  return buildBlueprints({ root, parseYaml: (text) => yaml.load(text), dumpYaml: (value) => yaml.dump(value) });
}

/** The built path a dev-server request names, or undefined when it is not a blueprint file. */
export function blueprintRequestPath(url) {
  const pathname = (url ?? '').split('?')[0] ?? '';
  const match = new RegExp(`(?:^|/)${BLUEPRINTS_DIR}/(.+)$`).exec(pathname);
  if (match === null) return undefined;
  const path = decodeURIComponent(match[1]);
  return path.split('/').includes('..') ? undefined : path;
}

export function blueprintsPlugin({ root = REPO_ROOT, build = defaultBuild } = {}) {
  let built;
  const files = () => {
    built ??= Promise.resolve(build(root)).then((result) => result.files);
    return built;
  };
  return {
    name: 'elitea-blueprints',
    async generateBundle() {
      for (const [path, source] of await files()) {
        this.emitFile({ type: 'asset', fileName: `${BLUEPRINTS_DIR}/${path}`, source });
      }
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const path = blueprintRequestPath(req.url);
        if (path === undefined) {
          next();
          return;
        }
        files().then(
          (map) => {
            const content = map.get(path);
            if (content === undefined) {
              next();
              return;
            }
            res.setHeader('Content-Type', blueprintContentType(path));
            res.setHeader('Cache-Control', 'no-cache');
            res.end(content);
          },
          (error) => next(error),
        );
      });
    },
  };
}
