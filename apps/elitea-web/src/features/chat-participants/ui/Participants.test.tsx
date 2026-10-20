/**
 * DEFECT: the participants rail could never render its "Users" row.
 *
 * `Participants` filtered the user group through `entities/participant`'s
 * `isParticipantStillActive` before handing it to the layout. That predicate
 * keys off the camelCase DOMAIN shape (`entityName`), while every row here is
 * the raw snake_case wire row (`entity_name`) `GET /elitea_core/conversation/
 * prompt_lib/{p}/{c}` returns — so it matched nothing and answered falsy for
 * all of them. And on the right shape it answers `false` for `user` anyway,
 * deliberately: the baseline's only caller is the last-message Regenerate
 * gate (`ChatMessageWrapper.jsx:148`), where a user is correctly "not
 * something a turn can be re-addressed to".
 *
 * The baseline rail (`ExpandedParticipantsList.jsx:50-56`) applies no
 * liveness check at all — user rows render there, filtered by `entity_name`
 * and sorted. These tests use the wire rows verbatim, `meta.user_name`
 * (overlaid server-side by `ListParticipants`) included, because a
 * camelCase-shaped fixture is exactly what hid the defect from the unit
 * suite in the first place.
 *
 * `useGetCurrentAuthor` (read by `ParticipantItemRow` for the self-exclusion
 * rule) is substituted at the network boundary per R-M1, the same wiring
 * `ExpandedParticipants/ParticipantItemRow.test.tsx` established.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { Participants } from './Participants';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

afterEach(() => {
  resetGeneratedClient();
});

/** One user participant exactly as the conversation payload states it. */
function userRow(id: string, userName: string): Record<string, unknown> {
  return { id: Number(id) + 100, entity_name: 'user', entity_meta: { id }, meta: { user_name: userName } };
}

function renderRail(participants: readonly Record<string, unknown>[], inspector = false): void {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: '1', name: 'Reader' })));

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <Participants participants={participants as never} inspector={inspector} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('Participants — the users row', () => {
  it('renders a user participant carried on the raw snake_case wire shape', async () => {
    renderRail([userRow('3', 'Bob Builder')]);

    await waitFor(() => {
      expect(screen.getByTestId('users-section')).toBeTruthy();
    });
    expect(screen.getByText('Bob Builder')).toBeTruthy();
  });

  it('renders the enriched name from meta.user_name, which only the wire shape spells', async () => {
    renderRail([userRow('3', 'Bob Builder'), userRow('4', 'Ada Lovelace')]);

    await waitFor(() => {
      expect(screen.getByTestId('users-section')).toBeTruthy();
    });
    expect(screen.getByText('Bob Builder')).toBeTruthy();
    expect(screen.getByText('Ada Lovelace')).toBeTruthy();
  });

  it('lists one user once when two participant rows name the same user', async () => {
    renderRail([userRow('3', 'Bob Builder'), { ...userRow('3', 'Bob Builder'), id: 999 }]);

    await waitFor(() => expect(screen.getByTestId('users-section')).toBeTruthy());
    expect(screen.getAllByText('Bob Builder')).toHaveLength(1);
  });

  it('stacks the users one per line in the desktop inspector', async () => {
    renderRail([userRow('3', 'Bob Builder'), userRow('4', 'Ada Lovelace')], true);

    const section = await screen.findByTestId('users-section');
    expect(getComputedStyle(section.firstElementChild as HTMLElement).flexDirection).toBe('column');
  });

  /** No user participants must still mean no row — the fix must not produce an empty one. */
  it('renders no users row when the conversation has no user participants', () => {
    renderRail([]);

    expect(screen.queryByTestId('users-section')).toBeNull();
    expect(screen.getByTestId('participants-container')).toBeTruthy();
  });
});
