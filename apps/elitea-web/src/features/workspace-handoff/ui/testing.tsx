/** Test helper: a client and a theme around the handoff UI. */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { RenderResult } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

export function renderWithClient(ui: React.ReactElement): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}
