/**
 * #6774: typing "#" in the composer opened a picker mode with no picker on
 * screen. These tests pin the picker itself: it searches with the text after
 * the "#", it says so when nothing matches, and a pick hands the "+" menu's
 * selection shape (with a pipeline marked as one) to the attach handler.
 */
import type { ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

const participantsState = vi.hoisted(() => ({
  lastParams: undefined as unknown,
  result: { participants: [] as unknown[], total: 0, isLoading: false, isFetching: false, isError: false, error: undefined },
}));

vi.mock('@/entities/participant', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/entities/participant')>()),
  useParticipants: (params: unknown) => {
    participantsState.lastParams = params;
    return participantsState.result;
  },
}));

import { HashParticipantPopup, toHashSelection } from './HashParticipantPopup';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function wrap(node: ReactNode): ReactNode {
  return <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>{node}</ThemeProvider>;
}

beforeEach(() => {
  participantsState.lastParams = undefined;
  participantsState.result = { participants: [], total: 0, isLoading: false, isFetching: false, isError: false, error: undefined };
});

describe('HashParticipantPopup (#6774)', () => {
  it('shows an empty state when the project has no agents or pipelines', () => {
    render(wrap(<HashParticipantPopup query="#" projectId="7" onSelect={vi.fn()} onClose={vi.fn()} />));
    expect(screen.getByTestId('chat-hash-participant-popup')).toBeInTheDocument();
    expect(screen.getByText('No matching results')).toBeInTheDocument();
  });

  it('searches agents and pipelines with the text after the "#"', () => {
    render(wrap(<HashParticipantPopup query="#rev" projectId="7" onSelect={vi.fn()} onClose={vi.fn()} />));
    expect(participantsState.lastParams).toMatchObject({ projectId: '7', query: 'rev', types: ['application'] });
  });

  it('hands a picked pipeline to the attach handler as a pipeline', async () => {
    participantsState.result = {
      ...participantsState.result,
      participants: [
        { label: 'Reviewer', participantType: 'application', isPublic: false, data: { id: 11, name: 'Reviewer', project_id: 7 } },
        { label: 'Release flow', participantType: 'pipeline', isPublic: false, data: { id: 12, name: 'Release flow', project_id: 7 } },
      ],
    };
    const onSelect = vi.fn();
    render(wrap(<HashParticipantPopup query="#re" projectId="7" onSelect={onSelect} onClose={vi.fn()} />));

    await userEvent.click(screen.getByText('Release flow'));

    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({
      id: '12', name: 'Release flow', participantType: 'pipeline', agent_type: 'pipeline', project_id: '7',
    }));
  });
});

describe('toHashSelection', () => {
  it('keeps an agent an agent and falls back to the chat project', () => {
    expect(toHashSelection({ label: 'A', participantType: 'application', isPublic: false, data: { id: '3' } }, '9'))
      .toStrictEqual({ id: '3', name: 'A', participantType: 'application', project_id: '9' });
  });
});
