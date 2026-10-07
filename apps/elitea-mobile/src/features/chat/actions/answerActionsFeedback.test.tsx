import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { feedbackEmpty, feedbackLiked } from '@/api/__fixtures__/chat13';
import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { planActions } from '@/features/chat/actions/messageActions';
import { SAMPLE_MESSAGES } from '@/features/designGallery/chatFixtures';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('react-native/Libraries/Utilities/useWindowDimensions', () => ({
  __esModule: true,
  default: () => ({ width: 393, height: 852, scale: 3, fontScale: 1 }),
}));

const P = 7;
const answers = SAMPLE_MESSAGES.filter((m) => m.role === 'assistant' && m.uuid !== null);
const route = (uuid: string) => `/elitea_core/message_feedback/prompt_lib/${P}/${uuid}`;
const noop = () => {};

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

describe('answer ratings in the action row', () => {
  it('reads the rating of the newest answer only, not one GET per rendered answer; Like never waits for it', async () => {
    expect(answers.length).toBeGreaterThanOrEqual(2);
    const server = fakeServer();
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    for (const a of answers) {
      server.on('GET', route(a.uuid!), { status: 200, body: feedbackEmpty });
      server.on('POST', route(a.uuid!), { status: 200, body: feedbackLiked });
    }
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
    const newest = answers[answers.length - 1];
    await render(
      <QueryClientProvider client={client}>
        {answers.map((message) => (
          <AnswerActions
            key={message.key}
            message={message}
            plan={planActions(message, { shareOut: true, contract13: true, online: true, canSpeak: true, canRegenerate: false })}
            projectId={P}
            reading={false}
            onReadAloud={noop}
            onStopReading={noop}
            onRegenerate={noop}
            onMore={noop}
            onDislike={noop}
            onNote={noop}
            loadRating={message.key === newest.key}
          />
        ))}
      </QueryClientProvider>,
    );
    const likes = screen.getAllByRole('button', { name: 'Like' });
    // Enabled at once, the newest one's read still in flight.
    for (const like of likes) expect(like.props.accessibilityState?.disabled).toBe(false);
    await waitFor(() => expect(server.requests.filter((r) => r.method === 'GET')).toHaveLength(1));
    expect(server.requests[0].url.pathname).toContain(newest.uuid!);
    // An older answer is rated without a read first.
    await fireEvent.press(likes[0]);
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST')).toBe(true));
    expect(server.requests.filter((r) => r.method === 'GET')).toHaveLength(1);
    // The rating landed (selected), so nothing is left in flight.
    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Like' })[0].props.accessibilityState?.selected).toBe(true));
    client.clear();
  });
});
