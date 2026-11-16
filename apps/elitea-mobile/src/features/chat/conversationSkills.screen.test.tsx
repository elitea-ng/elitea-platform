import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { fetchConversationSkills, skillPinErrorText, SkillPinError } from '@/api/queries/conversationSkills';
import { ParticipantsSheet } from '@/features/chat/ParticipantsSheet';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

// ADR-0050 amendment 1, A1.3: the Skills section sits below Participants, never among them.
const P = 7;
const me = { id: 1, entity_name: 'user', entity_meta: { id: 5 }, entity_settings: {}, meta: {} };
const PINS = '/elitea_core/conversation_skills/prompt_lib/7/3';
const pin = (id: number, name: string, extra: object = {}) => ({
  id,
  kind: 'project',
  skill_id: 100 + id,
  name,
  description: '',
  trust: { state: 'trusted' },
  pinned_by: 5,
  pinned_at: '2026-11-16T10:00:00Z',
  ...extra,
});

let queries: QueryClient;
let server: FakeServer;
const withQueries = (ui: ReactElement) => <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;
const sheet = () => (
  <ParticipantsSheet visible onClose={() => {}} projectId={P} conversationId="3" participants={[me] as never[]} selfUserId={5} offline={false} onChanged={() => {}} />
);

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
});
afterEach(() => queries.clear());

it('lists the pinned skills in their own section, with an untrusted note, and keeps them out of the participants', async () => {
  server.on('GET', PINS, {
    status: 200,
    body: { rows: [pin(1, 'Triage'), pin(2, 'Forked', { trust: { state: 'untrusted' } })], total: 2, max: 5 },
  });
  await render(withQueries(sheet()));
  expect(await screen.findByText(/^skills \(2\)/i)).toBeTruthy();
  expect(screen.getByText('Triage')).toBeTruthy();
  expect(screen.getByText(/Untrusted/)).toBeTruthy();
  // The participant list holds the person only.
  expect(screen.getAllByText(/\(you\)|Person/).length).toBeGreaterThan(0);
});

it('leaves the section out when the workspace has no conversation skills', async () => {
  await render(withQueries(sheet()));
  await waitFor(() => expect(server.requests.some((r) => r.url.pathname.endsWith('/conversation_skills/prompt_lib/7/3'))).toBe(true));
  expect(screen.queryByText(/^skills \(/i)).toBeNull();
});

it('removes a pin', async () => {
  server.on('GET', PINS, { status: 200, body: { rows: [pin(1, 'Triage')], total: 1, max: 5 } }, { status: 200, body: { rows: [], total: 0, max: 5 } });
  server.on('DELETE', '/elitea_core/conversation_skill/prompt_lib/7/3/1', { status: 204 });
  await render(withQueries(sheet()));
  await fireEvent.press(await screen.findByLabelText('Remove Triage from this conversation'));
  await waitFor(() => expect(server.requests.some((r) => r.method === 'DELETE' && r.url.pathname.endsWith('/conversation_skill/prompt_lib/7/3/1'))).toBe(true));
});

it('adds a project skill that is not pinned yet', async () => {
  server.on('GET', PINS, { status: 200, body: { rows: [pin(1, 'Triage')], total: 1, max: 5 } });
  server.on('GET', '/elitea_core/skills/prompt_lib/7', {
    status: 200,
    body: { items: [{ id: '101', name: 'Triage' }, { id: '200', name: 'Release notes' }], page: 1, page_size: 100, total: 2, total_pages: 1 },
  });
  server.on('POST', PINS, { status: 201, body: pin(9, 'Release notes', { skill_id: 200 }) });
  await render(withQueries(sheet()));
  await fireEvent.press(await screen.findByText('Add skill'));
  // Triage is pinned already, so only the other skill is offered.
  expect(screen.queryByLabelText('Add Triage')).toBeNull();
  await fireEvent.press(await screen.findByLabelText('Add Release notes'));
  await waitFor(() => {
    const post = server.requests.find((r) => r.method === 'POST');
    expect(post?.body).toBe(JSON.stringify({ skill_id: 200 }));
  });
});

it('says why a pin was refused', async () => {
  expect(skillPinErrorText(new SkillPinError('limit'))).toMatch(/at most five/);
  expect(skillPinErrorText(new SkillPinError('unavailable_skill'))).toMatch(/not available/);
  expect(skillPinErrorText(new SkillPinError('not_allowed'))).toMatch(/can't change/);
  expect(skillPinErrorText(new Error('x'))).toMatch(/Try again/);
  const refused = fakeServer();
  refused.on('GET', PINS, { status: 500 });
  await expect(fetchConversationSkills(P, '3', refused.client)).rejects.toMatchObject({ reason: 'failed' });
  const older = fakeServer();
  older.on('GET', PINS, { status: 404 });
  await expect(fetchConversationSkills(P, '3', older.client)).resolves.toBeNull();
});
