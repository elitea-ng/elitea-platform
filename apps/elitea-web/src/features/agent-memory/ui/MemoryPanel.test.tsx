import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { memoryEntry } from '../__tests__/fixtures';
import { renderWithMemoryProviders } from '../__tests__/testUtils';
import { MemoryPanel } from './MemoryPanel';

const BASE = '/api/v2';
const LIST = `${BASE}/elitea_core/agent_memory/prompt_lib/:projectId/:applicationId`;
const ENTRY = `${LIST}/:entryId`;

interface ListOptions {
  readonly rows?: readonly ReturnType<typeof memoryEntry>[];
  readonly manager?: boolean;
  readonly onQuery?: (params: URLSearchParams) => void;
}

function mockList({ rows = [memoryEntry({ status: 'pending' })], manager = true, onQuery }: ListOptions = {}): void {
  server.use(
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Ann' })),
    http.get(LIST, ({ request }) => {
      onQuery?.(new URL(request.url).searchParams);
      return HttpResponse.json({
        rows,
        total: rows.length,
        people: [{ id: 3, name: 'Ann' }],
        can_manage: manager,
        can_delete: manager,
        can_approve: manager,
        opted_out: false,
      });
    }),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('MemoryPanel', () => {
  it('asks to save the agent first', () => {
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={undefined} />);
    expect(screen.getByTestId('agent-memory-unsaved')).toBeInTheDocument();
  });

  it('lists the entries and approves a pending one', async () => {
    let patched: unknown;
    mockList();
    server.use(
      http.patch(ENTRY, async ({ request }) => {
        patched = await request.json();
        return HttpResponse.json(memoryEntry());
      }),
    );
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={41} />);
    expect(await screen.findByTestId('agent-memory-content-7')).toHaveTextContent('Prefers short answers');
    await userEvent.click(screen.getByTestId('agent-memory-approve-7'));
    await waitFor(() => expect(patched).toEqual({ status: 'active' }));
  });

  it('sends the search as the query', async () => {
    const queries: string[] = [];
    mockList({ onQuery: (params) => queries.push(params.get('query') ?? '') });
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={41} />);
    await screen.findByTestId('agent-memory-row-7');
    await userEvent.type(screen.getByTestId('agent-memory-search'), 'short');
    await waitFor(() => expect(queries).toContain('short'));
  });

  it('offers a person who is not a manager no Clear all and no Approve', async () => {
    mockList({ manager: false });
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={41} />);
    await screen.findByTestId('agent-memory-row-7');
    expect(screen.queryByTestId('agent-memory-clear')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-memory-approve-7')).not.toBeInTheDocument();
    // The entry is about this person (user 3), so they may still correct and forget it.
    expect(screen.getByTestId('agent-memory-edit-7')).toBeInTheDocument();
    expect(screen.getByTestId('agent-memory-delete-7')).toBeInTheDocument();
  });

  it('says so when nothing is remembered', async () => {
    mockList({ rows: [] });
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={41} />);
    expect(await screen.findByTestId('agent-memory-empty')).toBeInTheDocument();
  });

  it('turns off remembering things about the reader', async () => {
    let body: unknown;
    mockList();
    server.use(
      http.put(`${BASE}/elitea_core/agent_memory_opt_out/prompt_lib/:projectId/:applicationId`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ opted_out: true });
      }),
    );
    renderWithMemoryProviders(<MemoryPanel projectId="1" applicationId={41} />);
    await screen.findByTestId('agent-memory-row-7');
    await userEvent.click(within(screen.getByTestId('agent-memory-opt-out')).getByRole('switch'));
    await waitFor(() => expect(body).toEqual({ opted_out: true }));
  });
});
