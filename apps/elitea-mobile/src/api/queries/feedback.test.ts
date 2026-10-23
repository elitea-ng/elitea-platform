import { CAPTURE, feedbackBadRating, feedbackDisliked, feedbackEmpty, feedbackLiked } from '@/api/__fixtures__/chat13';
import { FeedbackError, MAX_FEEDBACK_COMMENT, clearFeedback, feedbackErrorText, fetchFeedback, setFeedback } from '@/api/queries/feedback';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = CAPTURE.projectId;
const M = CAPTURE.answerUuid;
const ROUTE = `/elitea_core/message_feedback/prompt_lib/${P}/${M}`;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

describe('message feedback (client contract 1.3, captured shapes)', () => {
  it('reads an unrated answer: counts and no "mine"', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 200, body: feedbackEmpty });
    await expect(fetchFeedback(P, M, server.client)).resolves.toEqual({ likes: 0, dislikes: 0, mine: null });
  });

  it('a dislike carries the comment; a like replaces it without one', async () => {
    const server = fakeServer();
    server.on('POST', ROUTE, { status: 200, body: feedbackDisliked }, { status: 200, body: feedbackLiked });
    await expect(setFeedback(P, M, -1, '  too short  ', server.client)).resolves.toEqual({
      likes: 0,
      dislikes: 1,
      mine: { rating: -1, comment: 'too short' },
    });
    await expect(setFeedback(P, M, 1, '', server.client)).resolves.toEqual({ likes: 1, dislikes: 0, mine: { rating: 1, comment: null } });
    const bodies = server.requests.filter((r) => r.method === 'POST').map((r) => JSON.parse(r.body!));
    expect(bodies).toEqual([{ rating: -1, comment: 'too short' }, { rating: 1 }]);
  });

  it('retracts the rating with DELETE', async () => {
    const server = fakeServer();
    server.on('DELETE', ROUTE, { status: 200, body: feedbackEmpty });
    await expect(clearFeedback(P, M, server.client)).resolves.toMatchObject({ mine: null });
    expect(server.requests[0].method).toBe('DELETE');
  });

  it("maps the server's 400 to 'invalid', 403 to 'not allowed', an older server to 'not available'", async () => {
    const server = fakeServer();
    server.on('POST', ROUTE, { status: 400, body: feedbackBadRating }, { status: 403, body: {} }, { status: 404, body: {} });
    await expect(setFeedback(P, M, -1, 'x', server.client)).rejects.toMatchObject({ reason: 'invalid' });
    await expect(setFeedback(P, M, -1, 'x', server.client)).rejects.toMatchObject({ reason: 'not_allowed' });
    const old = await setFeedback(P, M, -1, 'x', server.client).catch((e: unknown) => e);
    expect(old).toMatchObject({ reason: 'not_available' });
    expect(feedbackErrorText(old)).toMatch(/does not take feedback/);
  });

  it('refuses a comment the server would refuse, and a message id that is not a uuid, before any request', async () => {
    const server = fakeServer();
    await expect(setFeedback(P, M, -1, 'x'.repeat(MAX_FEEDBACK_COMMENT + 1), server.client)).rejects.toBeInstanceOf(FeedbackError);
    await expect(fetchFeedback(P, '../../admin', server.client)).rejects.toMatchObject({ reason: 'invalid' });
    expect(server.requests).toHaveLength(0);
  });

  it('a body that is not a summary is a failure, not a silent zero', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 200, body: { ok: true } });
    await expect(fetchFeedback(P, M, server.client)).rejects.toMatchObject({ reason: 'failed' });
  });
});
