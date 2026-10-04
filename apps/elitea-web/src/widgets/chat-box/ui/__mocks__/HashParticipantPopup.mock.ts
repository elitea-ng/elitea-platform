import { vi } from 'vitest';

interface ParticipantsState {
  lastParams: unknown;
  result: {
    participants: unknown[];
    total: number;
    isLoading: boolean;
    isFetching: boolean;
    isError: boolean;
    error: unknown;
  };
}

/** What the mocked `useParticipants` answers, and the params it was last called with. */
export const participantsState: ParticipantsState = {
  lastParams: undefined,
  result: { participants: [], total: 0, isLoading: false, isFetching: false, isError: false, error: undefined },
};

vi.mock('@/entities/participant', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/entities/participant')>()),
  useParticipants: (params: unknown) => {
    participantsState.lastParams = params;
    return participantsState.result;
  },
}));
