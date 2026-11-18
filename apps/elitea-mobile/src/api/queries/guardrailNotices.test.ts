import { fetchGuardrailNotices } from '@/api/queries/guardrailNotices';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const notice = (over: Record<string, unknown> = {}) => ({
  kind: 'redacted', stage: 'input', action: 'mask', guardrail: 'pii', detector: 'personal_data',
  labels: ['email'], requests: 1, first_seen_at: '2026-11-13T10:00:00Z', ...over,
});

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

describe('fetchGuardrailNotices (issue 2012)', () => {
  it('reads the typed notices of a run', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/guardrail_notices/prompt_lib/7/run-1', {
      status: 200,
      body: { run_id: 'run-1', notices: [notice(), notice({ kind: 'blocked', guardrail: 'keys', labels: ['github-token'] })] },
    });
    const notices = await fetchGuardrailNotices(7, 'run-1', server.client, undefined, 0);
    expect(notices.map((n) => [n.kind, n.guardrail, n.labels])).toEqual([
      ['redacted', 'pii', ['email']],
      ['blocked', 'keys', ['github-token']],
    ]);
    expect(server.requests).toHaveLength(1);
  });

  it('is an empty list when the read fails, so an answer is never broken by it', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/guardrail_notices/', { status: 500, body: { error: 'boom' } });
    expect(await fetchGuardrailNotices(7, 'run-1', server.client, undefined, 0)).toEqual([]);
    server.on('GET', '/elitea_core/guardrail_notices/', 'network_error');
    expect(await fetchGuardrailNotices(7, 'run-1', server.client, undefined, 0)).toEqual([]);
  });

  it('asks nothing for an id that is not a run id', async () => {
    const server = fakeServer();
    expect(await fetchGuardrailNotices(7, '../x', server.client, undefined, 0)).toEqual([]);
    expect(await fetchGuardrailNotices(0, 'run-1', server.client, undefined, 0)).toEqual([]);
    expect(server.requests).toHaveLength(0);
  });

  it('waits for the gateway to write its verdicts, and not at all once aborted', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/guardrail_notices/', { status: 200, body: { run_id: 'r', notices: [notice()] } });
    const controller = new AbortController();
    const pending = fetchGuardrailNotices(7, 'run-1', server.client, controller.signal, 60_000);
    controller.abort();
    expect(await pending).toEqual([]);
    expect(server.requests).toHaveLength(0);
  });
});
