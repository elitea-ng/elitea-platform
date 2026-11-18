import type { ReactElement, ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderHookOptions, RenderHookResult, RenderResult } from '@testing-library/react';
import { render, renderHook } from '@testing-library/react';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

/**
 * Local test-render helper for `features/agents` (test-only file; not part
 * of this slice's `index.ts` public API). A near-identical harness already
 * exists at `features/apps/__tests__/testUtils.tsx` (unit F5's Wave-2
 * partition), but `no-sideways-features` forbids importing across
 * `features/*` slices — including test-only files, which live under the
 * same `src/features/<slice>/` root the dependency-cruiser rule matches on
 * (verified against `.dependency-cruiser.cjs`'s `no-sideways-features`
 * pattern: `^src/features/([^/]+)/` with no test-path carve-out). Rebuilt
 * locally from `shared/brand`'s public exports rather than resolving that
 * boundary question, same call `features/apps/__tests__/testUtils.tsx`'s
 * own doc comment already made for its one internal `shared/ui/lib` import.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  });
}

function Providers({ children, queryClient }: { children: ReactNode; queryClient: QueryClient }) {
  return (
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        {children}
      </ThemeProvider>
    </QueryClientProvider>
  );
}

export function renderWithProviders(ui: ReactElement, queryClient: QueryClient = createTestQueryClient()): RenderResult {
  return render(<Providers queryClient={queryClient}>{ui}</Providers>);
}

export function renderHookWithProviders<TResult, TProps>(
  callback: (props: TProps) => TResult,
  queryClient: QueryClient = createTestQueryClient(),
  options?: Omit<RenderHookOptions<TProps>, 'wrapper'>,
): RenderHookResult<TResult, TProps> {
  return renderHook(callback, {
    ...options,
    wrapper: ({ children }: { children: ReactNode }) => <Providers queryClient={queryClient}>{children}</Providers>,
  });
}

/**
 * A real, COMPONENT-mounting router harness (not `renderHookWithRouter`,
 * which only captures a plain callback's return value into a probe — it
 * never renders a passed React element into the DOM; confirmed the hard
 * way, `AttachmentSwitch.test.tsx`/`ApplicationTools.test.tsx` — both need
 * `useSelectedProjectId()`, which throws outside ANY `<RouterProvider>`
 * ancestor — first tried `renderHookWithRouter`, got an empty `<div />`).
 * `Providers`' theme/query-client wrapping is included, unlike
 * `useSelectedProjectId.test.tsx`'s/`useIsFromApplication.test.tsx`'s own
 * minimal single-route harnesses (neither of those needs it).
 */
export function renderWithRouterAndProject(
  ui: ReactElement,
  projectId: string | undefined,
  queryClient: QueryClient = createTestQueryClient(),
  initialEntry = '/',
): RenderResult {
  function RootComponent(): ReactNode {
    return <Providers queryClient={queryClient}>{ui}</Providers>;
  }
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
  return render(<RouterProvider router={router} />);
}
