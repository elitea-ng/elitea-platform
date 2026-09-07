/**
 * Local test-render helper for `features/inventory-stats`.
 *
 * A near-identical harness exists in other feature slices, but
 * `no-sideways-features` forbids importing across `features/*` — including
 * test-only files, which live under the same `src/features/<slice>/` root the
 * dependency-cruiser rule matches on. Rebuilt locally from `shared/brand`'s
 * public exports, the same call the other slices' harnesses already made.
 */
import type { ReactElement, ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderHookOptions, RenderHookResult, RenderResult } from '@testing-library/react';
import { render, renderHook } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

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
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        {children}
      </ThemeProvider>
    </QueryClientProvider>
  );
}

export function renderWithProviders(
  ui: ReactElement,
  queryClient: QueryClient = createTestQueryClient(),
): RenderResult {
  return render(<Providers queryClient={queryClient}>{ui}</Providers>);
}

export function renderHookWithProviders<TResult, TProps>(
  callback: (props: TProps) => TResult,
  queryClient: QueryClient = createTestQueryClient(),
  options?: Omit<RenderHookOptions<TProps>, 'wrapper'>,
): RenderHookResult<TResult, TProps> {
  return renderHook(callback, {
    ...options,
    wrapper: ({ children }: { children: ReactNode }) => (
      <Providers queryClient={queryClient}>{children}</Providers>
    ),
  });
}
