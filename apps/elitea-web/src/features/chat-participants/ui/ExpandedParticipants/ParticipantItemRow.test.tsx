/**
 * Interpolation coverage for issue #45's stub migration.
 *
 * This row's accessible name is `t('chat-participants.row.mention', …)`.
 * Before #45 the file imported `t` from the always-return-the-fallback stub
 * `shared/ui/lib/t.ts`, so the JS template-literal fallback
 * (`` `Mention ${name}` ``) rendered the substituted text. After the
 * migration `t` resolves against `en.json`, whose value is
 * `"Mention {{name}}"` — the bundle wins, so the call site MUST pass the
 * matching options object or the accessible name becomes the literal
 * `"Mention {{name}}"` for every user row.
 *
 * The row had no test at all, so nothing failed when the migration exposed
 * this. Asserting on the accessible name keeps the check meaningful whatever
 * the key or bundle text later becomes.
 *
 * `useGetCurrentAuthor` is substituted at the network boundary (MSW) per
 * R-M1 — no `vi.mock` — using the same `QueryClientProvider` + generated-
 * client wiring `features/chat-input/ui/VoicePersonalizationSection.test.tsx`
 * established.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../../test/setup';

import ParticipantItemRow from './ParticipantItemRow';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

afterEach(() => {
  resetGeneratedClient();
});

/** Answers the one query this row makes (`useGetCurrentAuthor`) with the given id. */
function stubCurrentAuthor(id: string): void {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id, name: 'Signed-in User' })),
  );
}

function renderRow(participant: unknown): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <ParticipantItemRow
          participant={participant as never}
          isActive={false}
          onClickItem={vi.fn()}
        />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('ParticipantItemRow accessible name', () => {
  it('substitutes the participant name, rather than rendering a literal {{name}}', async () => {
    stubCurrentAuthor('me');
    renderRow({ entity_meta: { id: 'someone-else', name: 'Ada Lovelace' }, meta: {} });

    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Mention Ada Lovelace' })).toBeTruthy();
    });
    expect(screen.queryByRole('button', { name: /\{\{name\}\}/ })).toBeNull();
  });

  it('uses the bare name (no "Mention" prefix) for your own row, which is a click no-op', async () => {
    stubCurrentAuthor('me');
    renderRow({ entity_meta: { id: 'me', name: 'My Own Name' }, meta: {} });

    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'My Own Name' })).toBeTruthy();
    });
  });
});
