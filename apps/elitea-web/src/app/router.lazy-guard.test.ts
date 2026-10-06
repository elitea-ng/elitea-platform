// @vitest-environment node
/**
 * Build-side half of the unload-safe route chunk loading
 * (`shared/lib/chunk-load-guard.ts`): every auto-split route component must be
 * created by the GUARDED `lazyRouteComponent`, never TanStack's own.
 *
 * Runs `vite.config.ts`'s `guardedLazyRouteComponent` plugin and then the
 * real `@tanstack/router-plugin` code splitter over a route file, the same
 * order Vite runs them in. If a router-plugin upgrade stops reusing an
 * existing `lazyRouteComponent` binding, the reload-on-navigation bug comes
 * back silently; this test is what notices.
 */
import { fileURLToPath } from 'node:url';

import { tanStackRouterCodeSplitter } from '@tanstack/router-plugin/vite';
import { createRouterPluginContext } from '@tanstack/router-plugin/context';
import type { Plugin } from 'vite';
import { describe, expect, it } from 'vitest';

import { GUARDED_LAZY_ROUTE_COMPONENT_MODULE, guardedLazyRouteComponent } from '../../vite.config';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const ROUTES = fileURLToPath(new URL('../routes', import.meta.url));
const ROUTE_FILE = `${ROUTES}/example.tsx`;

const ROUTE_SOURCE = `import { createFileRoute } from '@tanstack/react-router';

function ExamplePage() {
  return null;
}

export const Route = createFileRoute('/example')({ component: ExamplePage });
`;

type TransformResult = { code: string } | null | undefined;
type Handler = (this: unknown, code: string, id: string) => TransformResult;

function transformOf(plugin: Plugin): Handler {
  const { transform } = plugin;
  if (!transform) throw new Error(`${plugin.name} has no transform hook`);
  return (typeof transform === 'function' ? transform : transform.handler) as Handler;
}

function referenceCompiler(): Handler {
  const context = createRouterPluginContext();
  context.routesByFile.set(ROUTE_FILE, { routeId: '/example' });
  const plugins = [
    tanStackRouterCodeSplitter(
      { target: 'react', autoCodeSplitting: true, routesDirectory: ROUTES, generatedRouteTree: `${ROOT}/src/routeTree.gen.ts` },
      context,
    ),
  ].flat();
  const reference = plugins.find((p) => p.name === 'tanstack-router:code-splitter:compile-reference-file');
  if (!reference) throw new Error('router-plugin no longer has its reference-file compiler');
  const configResolved = (reference as { vite?: { configResolved?: (c: unknown) => void } }).vite?.configResolved;
  configResolved?.({ command: 'build', root: ROOT, plugins: [] });
  return transformOf(reference);
}

const guardPlugin = guardedLazyRouteComponent(ROUTES) as Plugin;

describe('route chunks use the unload-guarded lazyRouteComponent', () => {
  it('the TanStack splitter calls the guarded binding, not its own import', () => {
    const guarded = transformOf(guardPlugin).call({}, ROUTE_SOURCE, ROUTE_FILE);
    expect(guarded?.code).toBeDefined();

    const compiled = referenceCompiler().call({}, guarded?.code ?? '', ROUTE_FILE);
    const code = compiled?.code ?? '';

    expect(code).toContain(`lazyRouteComponent($$splitComponentImporter, 'component')`);
    expect(code).toContain(JSON.stringify(GUARDED_LAZY_ROUTE_COMPONENT_MODULE).slice(1, -1));
    expect(code).not.toMatch(/import\s*\{[^}]*\blazyRouteComponent\b[^}]*\}\s*from\s*['"]@tanstack\/react-router['"]/);
  });

  it('prepends on the first line, so no line number moves', () => {
    const guarded = transformOf(guardPlugin).call({}, ROUTE_SOURCE, ROUTE_FILE);
    expect(guarded?.code.split('\n')).toHaveLength(ROUTE_SOURCE.split('\n').length);
  });

  it.each([
    ['a split virtual module', `${ROUTE_FILE}?tsr-split=component`, ROUTE_SOURCE],
    ['a file outside src/routes', `${ROOT}/src/pages/example.tsx`, ROUTE_SOURCE],
    ['a non-route helper', `${ROUTES}/-ui/helper.tsx`, 'export const x = 1;\n'],
    ['a file already binding lazyRouteComponent', ROUTE_FILE, `import { lazyRouteComponent } from 'x';\n${ROUTE_SOURCE}`],
  ])('leaves %s alone', (_name, id, code) => {
    expect(transformOf(guardPlugin).call({}, code, id)).toBeNull();
  });
});
