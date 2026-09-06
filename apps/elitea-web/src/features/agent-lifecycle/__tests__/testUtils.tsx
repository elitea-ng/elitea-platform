/**
 * Test providers for this slice.
 *
 * The same body as `features/skills/__tests__/testUtils.tsx`, duplicated for
 * the reason `no-sideways-features` forces: a feature slice may not import
 * another feature slice, and test helpers are not exempt from the layer rule.
 */
import type { ReactElement, ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, renderHook, type RenderHookResult, type RenderResult } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  });
}

function Providers({
  children,
  queryClient,
}: {
  readonly children: ReactNode;
  readonly queryClient: QueryClient;
}): ReactElement {
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

export function renderWithProviders(
  ui: ReactElement,
  queryClient: QueryClient = createTestQueryClient(),
): RenderResult {
  const result = render(<Providers queryClient={queryClient}>{ui}</Providers>);
  return {
    ...result,
    rerender: (nextUi: ReactNode) => {
      result.rerender(<Providers queryClient={queryClient}>{nextUi}</Providers>);
    },
  };
}

export function renderHookWithProviders<TResult>(
  callback: () => TResult,
  queryClient: QueryClient = createTestQueryClient(),
): RenderHookResult<TResult, unknown> {
  return renderHook(callback, {
    wrapper: ({ children }: { readonly children: ReactNode }) => (
      <Providers queryClient={queryClient}>{children}</Providers>
    ),
  });
}
