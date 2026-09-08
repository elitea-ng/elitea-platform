/*
 * The five toolkit route leaves, tested directly.
 *
 * These modules had NO test that imported them. `allRoutesSmoke.test.tsx`
 * mounts the whole generated tree and asserts each path settles, which proves
 * the leaves EXIST; it cannot tell a leaf that validates the right query keys
 * from one that validates none, nor a redirect that lands on `all` from one
 * that lands anywhere else. Both are single-line contracts, both are the kind
 * that is changed by a refactor and noticed by a user
 * ([[dead-code-with-no-caller-pattern]]).
 *
 * Written against the route objects themselves rather than through a mounted
 * router, because the contract under test is the route's OPTIONS: the redirect
 * target, the accepted query keys, and the presence of the pending/error
 * components a slow or failing loader falls back to.
 */
import type { ReactElement } from 'react';
import { Outlet } from '@tanstack/react-router';
import { describe, expect, it } from 'vitest';

import { Route as CreateRoute } from './create';
import { Route as CreateTypeRoute } from './create.$toolkitType';
import { Route as ToolkitsIndexRoute } from './index';
import { Route as TabRoute } from './$tab';
import { Route as ToolkitDetailRoute } from './$tab.$toolkitId';

/** The query keys `$tab` accepts. ROUTE-029's PARAM-094..106 plus `view`. */
const TAB_PARAMS = [
  'destTab',
  'edited_participant_id',
  'forceCustom',
  'name',
  'newToolkitId',
  'return_url',
  'source_application_id',
  'view',
] as const;

/**
 * `pickParams` returns a zod object, which TanStack Router accepts as a
 * `validateSearch` "standard schema" rather than as a plain function — so the
 * route's own declaration is parsed here the way the router parses it.
 */
function validate(route: { options: { validateSearch?: unknown } }, search: Record<string, unknown>) {
  const validateSearch = route.options.validateSearch as
    | { parse: (input: Record<string, unknown>) => Record<string, unknown> }
    | undefined;
  expect(validateSearch, 'the route declares no validateSearch').toBeDefined();
  expect(validateSearch?.parse, 'validateSearch is not a parseable schema').toBeTypeOf('function');
  return (validateSearch as { parse: (input: Record<string, unknown>) => Record<string, unknown> }).parse(search);
}

/** Calls a route's component function, producing its element tree uninvoked. */
function renderElement(route: { options: { component?: unknown } }): ReactElement {
  const component = route.options.component as () => ReactElement;
  expect(component).toBeTypeOf('function');
  return component();
}

function componentNameOf(element: ReactElement): string {
  const type = element.type as { name?: string; displayName?: string } | string;
  if (typeof type === 'string') return type;
  return type.displayName ?? type.name ?? '';
}

/** The elements one element renders directly, one level down. */
function childElements(element: ReactElement): readonly ReactElement[] {
  const children = (element.props as { children?: unknown }).children;
  const list = Array.isArray(children) ? children : [children];
  return list.filter((child): child is ReactElement => child !== null && typeof child === 'object');
}

function childComponentNames(element: ReactElement): readonly string[] {
  return childElements(element).map(componentNameOf);
}

/**
 * Whether one element renders the router's `Outlet` directly.
 *
 * By IDENTITY, not by name: `Outlet` is an anonymous export in the installed
 * @tanstack/react-router build, so a name comparison reads `''` and would pass
 * for any other anonymous child.
 */
function rendersOutlet(element: ReactElement): boolean {
  return childElements(element).some((child) => child.type === Outlet);
}

describe('/toolkits route leaves', () => {
  it('redirects the bare /toolkits path to the first tab', () => {
    const beforeLoad = ToolkitsIndexRoute.options.beforeLoad as (() => void) | undefined;
    expect(beforeLoad).toBeTypeOf('function');

    let thrown: unknown;
    try {
      (beforeLoad as () => void)();
    } catch (error) {
      thrown = error;
    }
    // TanStack Router's redirect() returns a Response subclass carrying the
    // target under `.options`, and beforeLoad must THROW it. A `return` here
    // would leave the index route rendering nothing, which the user sees as a
    // blank page.
    expect(thrown, '/toolkits must throw a redirect, not return one').toBeInstanceOf(Response);
    const redirect = (thrown as { options: { to?: string; params?: { tab?: string } } }).options;
    expect(redirect.to).toBe('/toolkits/$tab');
    // `all` and not, say, `my-liked`: ToolkitsTabs' first entry is the one the
    // list page's own empty-list redirect and every in-app link assume.
    expect(redirect.params?.tab).toBe('all');
  });

  it('accepts every declared query key on /toolkits/:tab and drops the rest', () => {
    const supplied: Record<string, unknown> = {};
    for (const key of TAB_PARAMS) supplied[key] = key === 'forceCustom' ? '1' : `value-${key}`;
    supplied['an_undeclared_key'] = 'must not survive';

    const validated = validate(TabRoute, supplied);
    for (const key of TAB_PARAMS) {
      expect(Object.keys(validated), `${key} is declared by ROUTE-029 and must be accepted`).toContain(key);
    }
    expect(Object.keys(validated)).not.toContain('an_undeclared_key');
  });

  it('accepts index_name on the toolkit detail route, and nothing else', () => {
    const validated = validate(ToolkitDetailRoute, {
      index_name: 'docs',
      // The parent's keys are validated by the PARENT match, not this one.
      // Declaring them here as well would let a detail URL carry a second,
      // independently-parsed copy of a value the parent already owns.
      destTab: 'History',
      unexpected: 1,
    });
    expect(Object.keys(validated)).toContain('index_name');
    expect(validated['index_name']).toBe('docs');
    expect(Object.keys(validated)).not.toContain('unexpected');
  });

  it('gives every rendering toolkit route a pending and an error component', () => {
    // A route with neither shows a blank screen while its loader runs and a
    // whole-app error boundary when it fails. Both are the shell's job here.
    for (const [name, route] of [
      ['$tab', TabRoute],
      ['$tab/$toolkitId', ToolkitDetailRoute],
      ['create', CreateRoute],
    ] as const) {
      expect(route.options.component, `${name} renders nothing`).toBeTypeOf('function');
      expect(route.options.pendingComponent, `${name} has no pending component`).toBeTypeOf('function');
      expect(route.options.errorComponent, `${name} has no error component`).toBeTypeOf('function');
    }
  });

  /*
   * WHICH PAGE each leaf mounts, and what it wraps it in.
   *
   * These three route components were the only executable code in this
   * directory and NOTHING ran them: the whole-tree smoke test mounts the
   * router but never reaches them, so before this test every one of the three
   * reported zero covered functions. A leaf repointed at the wrong page — the
   * copy-paste this shape invites — would have shipped.
   *
   * The component FUNCTION is called, not rendered. Calling it builds the
   * element tree without invoking any child, so the assertion is about the
   * wiring and needs none of the three pages' providers.
   */
  it('mounts the page each leaf exists for, in the wrapper that leaf needs', () => {
    const tabTree = renderElement(TabRoute);
    // ExclusiveOutlet, not a fragment: `/toolkits/$tab` is the structural
    // PARENT of `/toolkits/$tab/$toolkitId`, and without it the list and the
    // detail render nested instead of exclusively.
    expect(componentNameOf(tabTree)).toBe('ExclusiveOutlet');
    expect(childComponentNames(tabTree)).toContain('Toolkits');

    const detailTree = renderElement(ToolkitDetailRoute);
    // A fragment with the page and an Outlet: the detail route's own child is
    // a param-only refinement, so the detail screen keeps rendering under it.
    expect(childComponentNames(detailTree)).toContain('EditToolkit');
    expect(rendersOutlet(detailTree), 'the detail route renders no Outlet').toBe(true);

    const createTree = renderElement(CreateRoute);
    expect(childComponentNames(createTree)).toContain('CreateToolkit');
    expect(rendersOutlet(createTree), 'the create route renders no Outlet').toBe(true);
  });

  it('leaves /toolkits/create/:toolkitType as a bare child of create', () => {
    // ROUTE-028 is deliberately empty: the type chooser lives on the PARENT
    // and the child exists only so the picked type is addressable in the URL.
    // A component added here would render a second create screen underneath
    // the first one.
    expect(CreateTypeRoute.options.component).toBeUndefined();
    expect(CreateTypeRoute.options.validateSearch).toBeUndefined();
  });

});
