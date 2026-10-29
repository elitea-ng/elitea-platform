import { cpSync, readFileSync } from 'node:fs';
import { URL, fileURLToPath } from 'node:url';

import babel from '@rolldown/plugin-babel';
import { tanstackRouter } from '@tanstack/router-plugin/vite';
import react, { reactCompilerPreset } from '@vitejs/plugin-react';
import { defineConfig, searchForWorkspaceRoot, type PluginOption, type UserConfig } from 'vite';

// No `.ts` extension: tsconfig.json does not set `allowImportingTsExtensions`
// (TS5097), so adding one here to silence Vite's own `configLoader: 'native'`
// forward-compat notice would break `tsc --noEmit`. Vite resolves the
// extensionless form today regardless of that loader setting.
import { nestCatalogue } from './src/shared/i18n/nestCatalogue';
import { viteSingleFile } from 'vite-plugin-singlefile';
import svgr from 'vite-plugin-svgr';
import tsconfigPaths from 'vite-tsconfig-paths';

const resolvePath = (p: string): string => fileURLToPath(new URL(p, import.meta.url));

/**
 * Emit the i18n catalogue (`src/shared/i18n/en.json`) as a tree, keys sorted.
 *
 * The whole catalogue ships in the initial set of both the app and the
 * admin entry (see shared/i18n/README.md — one bundle, no namespace split),
 * so its gzip size is initial-bundle size. Two steps, both at build time so
 * en.json itself never takes a whole-file rewrite:
 *  - sorted: the file keeps the order `scripts/i18n-backfill.mjs` preserves
 *    (new keys appended), which leaves later-added keys away from their
 *    dotted-prefix neighbours. Measured 2026-10-04, sorting alone took the
 *    admin initial set down 0.6 KiB gzip.
 *  - nested: each dotted prefix is written once instead of once per key
 *    (`src/shared/i18n/nestCatalogue.ts`). Measured 2026-10-05, the catalogue
 *    goes from 68.2 to 59.6 KiB gzip. i18next reads `t('a.b.c')` from either
 *    shape, and the keys that cannot nest stay flat; `nestCatalogue.test.ts`
 *    resolves every key through a real i18next instance to prove it.
 * This changes bytes, not behaviour. Unit tests (vitest's own plugin list)
 * still read the flat file.
 */
const compactI18nCatalogue = (): PluginOption => ({
  name: 'elitea:compact-i18n-catalogue',
  enforce: 'pre',
  transform(code, id) {
    if (!id.endsWith('/src/shared/i18n/en.json')) return null;
    return { code: JSON.stringify(nestCatalogue(JSON.parse(code) as Record<string, string>)), map: null };
  },
});

/**
 * Route chunks load through `shared/lib/chunk-load-guard`'s
 * `lazyRouteComponent`, not TanStack's, so a chunk import that fails because
 * the page is being navigated away from cannot reload the page and replace
 * that navigation (WebKit: "Importing a module script failed." →
 * `window.location.reload()` → Playwright `page.goto: Frame load
 * interrupted`, and a bounced navigation in Safari). See that module's header.
 *
 * Mechanism: the router plugin's code splitter only adds
 * `import { lazyRouteComponent } from '@tanstack/react-router'` when the
 * route file has no `lazyRouteComponent` binding yet (installed
 * `@tanstack/router-plugin@1.168.42`, `core/code-splitter/compilers.js`,
 * `hasImportedOrDefinedIdentifier` → `scope.hasBinding`), and then calls
 * whatever that binding is. This plugin runs just before it and provides the
 * binding. The import goes on the file's first line so no line number moves.
 * `src/app/router.lazy-guard.test.ts` runs the real splitter over a route
 * file to pin that contract against a router-plugin upgrade. A route file
 * that imports its own `lazyRouteComponent` is left alone.
 */
export const GUARDED_LAZY_ROUTE_COMPONENT_MODULE = resolvePath('./src/shared/lib/chunk-load-guard.ts');

export const guardedLazyRouteComponent = (routesDirectory: string): PluginOption => ({
  name: 'elitea:guarded-lazy-route-component',
  enforce: 'pre',
  transform(code, id) {
    if (id.includes('?') || !id.startsWith(`${routesDirectory}/`) || !/\.tsx?$/.test(id)) return null;
    if (!code.includes('createFileRoute') || /import\s*\{[^}]*\blazyRouteComponent\b/.test(code)) return null;
    return {
      code: `import { lazyRouteComponent } from ${JSON.stringify(GUARDED_LAZY_ROUTE_COMPONENT_MODULE)};${code}`,
      map: null,
    };
  },
});

/**
 * pdf.js's plain-JavaScript JPEG 2000 and JBIG2 decoders, next to its worker.
 *
 * The Artifacts PDF preview runs pdf.js with WebAssembly off (neither CSP
 * allows 'wasm-unsafe-eval'), so pdf.js falls back to these two modules,
 * which it `import()`s as `${wasmUrl}<fixed file name>`. A `?url` import would
 * hash the names, so they keep their own names in `assets/pdfjs-<version>/`,
 * beside the worker, and `features/artifacts/lib/pdfjs.ts` points `wasmUrl`
 * there. The version folder stands in for the hash: nginx serves `assets/` as
 * immutable for a year, so an upgrade must change the URL. Same origin, no
 * eval: both CSPs' `script-src 'self'` admit them. The dev server serves them
 * from the package itself.
 */
const PDFJS_NOWASM_DECODERS = ['openjpeg_nowasm_fallback.js', 'jbig2_nowasm_fallback.js'];
const PDFJS_VERSION = (
  JSON.parse(readFileSync(resolvePath('./node_modules/pdfjs-dist/package.json'), 'utf8')) as { version: string }
).version;
const pdfjsNoWasmDecoders = (): PluginOption => ({
  name: 'elitea:pdfjs-nowasm-decoders',
  apply: 'build',
  generateBundle() {
    for (const name of PDFJS_NOWASM_DECODERS) {
      this.emitFile({
        type: 'asset',
        fileName: `assets/pdfjs-${PDFJS_VERSION}/${name}`,
        source: readFileSync(resolvePath(`./node_modules/pdfjs-dist/wasm/${name}`)),
      });
    }
  },
});

/**
 * The main SPA's plugin chain (route tree + guarded lazy routes + the base
 * plugins), shared by the default build and the `desktop` build.
 *
 * Must run before @vitejs/plugin-react (react()): the router plugin rewrites
 * route files' exports (autoCodeSplitting) before JSX/compiler transforms see
 * them. Verified via the installed package's own vite.js plugin ordering
 * example. `guardedLazyRouteComponent` must run before it.
 */
function mainAppPlugins(basePlugins: PluginOption[]): PluginOption[] {
  return [
    guardedLazyRouteComponent(resolvePath('./src/routes')),
    tanstackRouter({
      target: 'react',
      autoCodeSplitting: true,
      routesDirectory: resolvePath('./src/routes'),
      generatedRouteTree: resolvePath('./src/routeTree.gen.ts'),
      // Matches `__tests__/` directories AND colocated `*.test.tsx`
      // files (e.g. `routes/auth-callback.test.tsx`) — both patterns
      // appear under src/routes/**; without the second alternative the
      // generator warns "does not export a Route" for every colocated
      // test file (harmless — they're excluded from the tree either
      // way — but noisy, and worth silencing at the source).
      routeFileIgnorePattern: '(__tests__|\\.test\\.tsx?$)',
      codeSplittingOptions: {
        splitBehavior: ({ routeId }) => (routeId === '/auth-callback' ? [] : undefined),
      },
    }),
    ...basePlugins,
    pdfjsNoWasmDecoders(),
  ];
}

/**
 * Six build targets (spec §7.4, ADR-0029), selected via `vite build --mode <target>`:
 *
 *  - (default)      main SPA        base './' (contract C4), outDir dist/app
 *  - admin          admin module    base '/admin/app/' so the Go adminui handler's
 *                                   marker substitution and asset serving keep
 *                                   working unchanged (contract C15), dist/admin
 *  - maintenance    self-contained  vite-plugin-singlefile inlines everything into
 *                                   one maintenance.html, dist/maintenance
 *  - brand-preview  self-contained  the offline brand previewer (ADR-0024 WP9),
 *                                   one index.html, dist/brand-preview
 *  - desktop        the Tauri desktop client's bundled UI (ADR-0029 decision 9):
 *                   the main app behind entries/desktop, base './', dist-desktop
 *                   (outside dist/ on purpose: the bundle-budget gate reads dist/
 *                   and must keep seeing exactly the targets `npm run build` makes)
 *
 * Vite keeps NODE_ENV=production for `vite build` regardless of a custom --mode
 * (mode and NODE_ENV are distinct), so all three outputs are production builds.
 *
 * The TanStack Router plugin (unit R1, §2.3/§9.3): file-based routing over
 * `src/routes/**`, `autoCodeSplitting: true`. Main-app target ONLY — admin
 * and maintenance are plain single-entry SPAs with no route tree.
 * `routeFileIgnorePattern` excludes `-`-prefixed helper directories
 * (`-search/`, `-guards/`, `-ui/`) by TanStack's own default convention
 * (`routeFileIgnorePrefix` defaults to `"-"`, verified against the
 * installed `@tanstack/router-generator@1.168.23` — no override needed) and
 * `__tests__/`, which is NOT excluded by default and must be listed
 * explicitly.
 *
 * `splitBehavior` keeps `/auth-callback` OUT of the split (`[]` = no lazy
 * chunk for that route, matching old `router.jsx:4`'s eager, non-lazy
 * import of `AuthCallbackPage` — every other page in the old app is
 * `ChunkHelpers.lazyWithRetry`-wrapped, this one deliberately is not).
 */
export default defineConfig(({ mode }): UserConfig => withClientCoreAccess(configFor(mode)));

/**
 * `@elitea/client-core` (libs/ts/client-core) is TypeScript source outside
 * this app's root: `tsconfigPaths()` resolves the import and Vite compiles
 * it, but the dev server serves only allowed directories. Every mode's dev
 * server allows that package on top of whatever it already allowed; a build
 * is not affected.
 *
 * Merged, not replaced: setting `server.fs.allow` drops Vite's own default
 * (the workspace root), so that default is put back first, then any `fs`
 * options and `allow` entries the config already had. vitest.config.ts
 * applies the same function, so the storybook project's browser-mode server
 * can serve the package too.
 */
export function withClientCoreAccess<T extends UserConfig>(config: T): T {
  const fs = config.server?.fs;
  return {
    ...config,
    server: {
      ...config.server,
      fs: {
        ...fs,
        allow: [searchForWorkspaceRoot(process.cwd()), ...(fs?.allow ?? []), resolvePath('../../libs/ts/client-core')],
      },
    },
  };
}

function configFor(mode: string): UserConfig {
  const basePlugins: PluginOption[] = [
    compactI18nCatalogue(),
    react(),
    // React Compiler (spec §2.1): plugin-react 6 removed its `babel` option, so
    // the compiler is wired via @rolldown/plugin-babel + reactCompilerPreset().
    babel({ presets: [reactCompilerPreset()] }),
    // svgrOptions.ref (unit S2, §3.7/R-T8): every icon in shared/ui/icons/** is imported via
    // the `?react` convention and typed as a ref-forwarding component (see
    // shared/ui/icons/svg-react.d.ts, which overrides the package's default,
    // non-ref-forwarding `vite-plugin-svgr/client` typings to match).
    svgr({ svgrOptions: { ref: true } }),
    tsconfigPaths({ projects: [resolvePath('./tsconfig.json')] }),
  ];

  if (mode === 'admin') {
    return {
      root: resolvePath('./src/entries/admin'),
      base: '/admin/app/',
      // Copied verbatim into dist/admin. Holds `assets/scheme-init.js` and
      // `assets/favicon.svg` (ADR-0024 WP3): the Go adminui handler serves
      // exactly one directory from disk, `/admin/app/assets/*`, and its CSP
      // admits no second inline script, so the first-paint scheme script
      // has to be a same-origin FILE under that directory.
      publicDir: resolvePath('./src/entries/admin/public'),
      // The default pack's fontFaces are `./brand/fonts/*.woff2`. The console
      // resolves them against its base (`/admin/app/brand/fonts/…`, see
      // shared/brand/fontFaces.ts) and the adminui handler serves `/brand/*`
      // from dist/admin/brand, so the typefaces are copied there rather than
      // into a second publicDir copy.
      plugins: [
        ...basePlugins,
        {
          name: 'elitea-admin-brand-fonts',
          apply: 'build',
          closeBundle() {
            cpSync(resolvePath('./public/brand/fonts'), resolvePath('./dist/admin/brand/fonts'), { recursive: true });
          },
        },
      ],
      build: {
        outDir: resolvePath('./dist/admin'),
        emptyOutDir: true,
        sourcemap: false,
      },
    };
  }

  if (mode === 'maintenance') {
    return {
      plugins: [...basePlugins, viteSingleFile()],
      root: resolvePath('./src/entries/maintenance'),
      base: './',
      publicDir: false,
      build: {
        outDir: resolvePath('./dist/maintenance'),
        emptyOutDir: true,
        sourcemap: false,
        rollupOptions: {
          input: resolvePath('./src/entries/maintenance/maintenance.html'),
        },
      },
    };
  }

  if (mode === 'brand-preview') {
    // ADR-0024 WP9: the offline brand previewer a branding package ships as
    // `preview/app.html`. Same shape as the maintenance entry — one HTML
    // file with every script and style inlined — and, unlike it, with NO
    // request of its own: a designer opens it from `file://` with no Elitea
    // running, and the Go exporter fills the inline `#brand-pack` element.
    return {
      plugins: [...basePlugins, viteSingleFile()],
      root: resolvePath('./src/entries/brand-preview'),
      base: './',
      publicDir: false,
      build: {
        outDir: resolvePath('./dist/brand-preview'),
        emptyOutDir: true,
        sourcemap: false,
        rollupOptions: {
          input: resolvePath('./src/entries/brand-preview/index.html'),
        },
      },
    };
  }

  if (mode === 'desktop') {
    // ADR-0029 decision 9: the app the Tauri host loads from its BUNDLED assets.
    // Same route tree, plugins and brand as the default SPA — every cloud screen
    // is reused — behind a different entry (`entries/desktop`: connect screen,
    // then the real app) and an absolute-URL/bearer transport that the entry
    // registers at runtime. Nothing here changes the default build: the desktop
    // modules are reachable only from that entry, so they are not in dist/app.
    return {
      plugins: mainAppPlugins(basePlugins),
      root: resolvePath('./src/entries/desktop'),
      base: './',
      publicDir: resolvePath('./public'),
      build: {
        outDir: resolvePath('./dist-desktop'),
        emptyOutDir: true,
        sourcemap: false,
      },
    };
  }

  return {
    plugins: mainAppPlugins(basePlugins),
    base: './', // contract C4: assets emitted with relative URLs
    build: {
      outDir: resolvePath('./dist/app'),
      emptyOutDir: true,
      sourcemap: false,
    },
    server: devServerProxy(),
  };
}

/**
 * `npm run dev` against a real backend, with HMR.
 *
 * The SPA is normally served BY elitea-main, so it has no API of its own to
 * talk to in dev. This forwards every server-owned path to a running stack —
 * by default the standalone one (`deploy/scripts/standalone-stack.sh up`) at
 * :8084 — so `npm run dev` gives hot reload against real data instead of a
 * blank screen.
 *
 * Point it elsewhere with `ELITEA_DEV_BACKEND=http://host:port npm run dev`.
 *
 * The four proxied prefixes are not arbitrary:
 *
 *  - `/api/v2`      the whole REST surface, AND `branding/bootstrap.js`, which
 *                   `index.html` loads with a ROOT-ABSOLUTE src. Without this
 *                   the brand pack 404s and the app silently falls back to the
 *                   compiled-in default (channel A) — it would look almost
 *                   right, which is worse than looking broken.
 *  - `config.js`    at ANY depth — `index.html` asks for it RELATIVE, so a
 *                   deep link resolves it against that path. The stack serves
 *                   the same file at `/app/config.js`, hence the rewrite. It
 *                   carries `vite_server_url`, `vite_public_project_id` and
 *                   the socket settings; with it missing the app boots with an
 *                   undefined config rather than an error.
 *  - `/auth`         the sign-in redirect chain.
 *  - `/socket.io`   upgraded, not just forwarded (`ws: true`).
 *
 * Cookies work across the port change because a cookie's origin is its DOMAIN,
 * not its port: a session minted at localhost:8084 is sent to localhost:5173
 * too. That is why this needs no cookie rewriting and why signing in on either
 * port signs you in on both.
 */
function devServerProxy(): NonNullable<UserConfig['server']> {
  const target = process.env['ELITEA_DEV_BACKEND'] ?? 'http://localhost:8084';
  const forward = { target, changeOrigin: true } as const;
  return {
    proxy: {
      '/api/v2': forward,
      '/llm': forward,
      '/auth': forward,
      '/socket.io': { ...forward, ws: true },
      // A REGEX, matching `config.js` at ANY depth. `index.html` loads it as
      // `./config.js`, so the browser resolves it against the current path:
      // `/settings/personalization` asks for `/settings/config.js`, not
      // `/config.js`. In production nginx absorbs that with a depth-tolerant
      // alias (contract C8); a literal `/config.js` key here works on the root
      // route and then hands every deep link a 404 that surfaces as the
      // "System env missing: VITE_SERVER_URL …" page.
      '^/.*/?config\\.js$': { ...forward, rewrite: () => '/app/config.js' },
    },
  };
}
