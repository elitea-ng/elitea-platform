import { createContext, useContext, type ReactElement, type ReactNode } from 'react';

import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter, type AnyRouter } from '@tanstack/react-router';
import { render, type RenderResult } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { PageChromeProvider, useShellCrumbs } from '@/shared/ui/page-chrome';

/**
 * The credential screens under a one-route router: the type gallery's search
 * box (`widgets/page-header`'s `PageSearch`) reads and writes the URL's
 * `query`, and throws outside a router.
 *
 * `rerender(nextUi)` swaps the content inside the SAME router (through a
 * context the route reads), so a test can feed a new `mode` to a mounted
 * form the way the route does.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const ContentContext = createContext<ReactNode>(null);

function RouteContent(): ReactNode {
  return useContext(ContentContext);
}

export interface RenderCredentialsRouteResult extends RenderResult {
  readonly router: AnyRouter;
}

export function renderCredentialsRoute(ui: ReactElement, initialPath = '/credentials/create-credential'): RenderCredentialsRouteResult {
  const router = createRouter({
    routeTree: createRootRoute({ component: RouteContent }),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
  const wrap = (content: ReactNode): ReactElement => (
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <CssBaseline />
      <ContentContext.Provider value={content}>
        <RouterProvider router={router} />
      </ContentContext.Provider>
    </ThemeProvider>
  );
  const result = render(wrap(ui));
  return {
    ...result,
    router,
    rerender: (nextUi: ReactNode) => {
      result.rerender(wrap(nextUi));
    },
  };
}

function CrumbProbe(): ReactNode {
  return <div data-testid="crumbs">{useShellCrumbs().map((crumb) => crumb.label).join(' / ')}</div>;
}

/** `ui` under the shell's crumb store, with the crumbs it registers in `data-testid="crumbs"`. */
export function withShellCrumbs(ui: ReactNode): ReactElement {
  return (
    <PageChromeProvider>
      {ui}
      <CrumbProbe />
    </PageChromeProvider>
  );
}
