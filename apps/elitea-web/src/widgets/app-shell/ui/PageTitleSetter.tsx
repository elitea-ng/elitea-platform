import { useEffect, useMemo } from 'react';

import { useRouterState } from '@tanstack/react-router';

import { productFullName } from '@/shared/brand';

import { derivePageTitle } from '../lib/pageTitle';

export interface PageTitleSetterProps {
  projectName: string | undefined;
}

/**
 * Ported from `hooks/useBrowserPageTitle.js`'s `PageTitleSetter`
 * (reduced-scope notes: `../lib/pageTitle.ts`). Renders nothing.
 *
 * The brand pack's `product.name` is appended to every derived title. In the
 * old app the product name reached the browser exactly once — the static
 * `<title>Elitea</title>` in `index.html` — and the first route change
 * overwrote it, because `useBrowserPageTitle` composes titles from the
 * section and the project only. That is why JRNY-030 ("logo, primary colour
 * and product name come from the pack with no rebuild") had nothing
 * observable to assert on for the name: the one place it appeared was a
 * compiled-in literal that never survived navigation. Reading it from the
 * resolved pack here is what makes the name genuinely pack-driven, and the
 * suffix is the only visible change for a deployment on the default pack
 * (`… - Elitea`).
 *
 * `productFullName()` (the resolved pack's `product.name`) is memoised
 * rather than called per render: it runs a zod parse over the served pack, and the global it reads is fixed for the
 * document's lifetime (`index.html` sets it with a blocking script before the
 * bundle evaluates).
 */
export function PageTitleSetter({ projectName }: PageTitleSetterProps): null {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const searchName = useRouterState({
    select: (routerState) => {
      const search = routerState.location.search as Record<string, unknown> | undefined;
      const value = search?.['name'];
      return typeof value === 'string' ? value : '';
    },
  });
  const productName = useMemo(() => productFullName(), []);

  useEffect(() => {
    const derived = derivePageTitle(pathname, searchName, projectName);
    // `derived` is the project name alone (possibly '') for a pathname no
    // section matches — see `derivePageTitle`. Joining unconditionally would
    // produce a leading " - " in that case.
    document.title = derived === '' ? productName : `${derived} - ${productName}`;
  }, [pathname, searchName, projectName, productName]);

  return null;
}
