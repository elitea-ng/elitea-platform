/**
 * Coverage for `RequestProjectDialog` (#871) — the member-facing half of the
 * self-service "request a project" flow. Exercises the real generated
 * client through MSW, the same harness `Webhooks.test.tsx` uses for the
 * sibling project-scoped feature.
 */
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { RequestProjectDialog } from './RequestProjectDialog';

const BASE = '/api/v2';
const MINE_PATH = `${BASE}/admin/moderation_status/project_requests/mine`;
const CREATE_PATH = `${BASE}/admin/moderation_status/project_request`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

function mount(open = true) {
  return render(
    <AppProviders>
      <RequestProjectDialog open={open} onClose={() => undefined} />
    </AppProviders>,
  );
}

describe('RequestProjectDialog — history', () => {
  it("shows 'no requests yet' for a caller with none", async () => {
    server.use(http.get(MINE_PATH, () => HttpResponse.json({ total: 0, rows: [] })));

    mount();

    await waitFor(() => {
      expect(screen.getByText("You haven't requested a project yet.")).toBeInTheDocument();
    });
  });

  it('renders past requests with their status and, for an approved one, the created project id', async () => {
    server.use(
      http.get(MINE_PATH, () =>
        HttpResponse.json({
          total: 2,
          rows: [
            {
              id: 1,
              user_id: 10,
              user_email: 'ada@example.com',
              project_id: 2,
              issue_type: 'Project Request',
              entity_id: 'Marketing Automation',
              description: 'need it',
              status: 'approved',
              rejection_comment: null,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              created_project_id: 42,
            },
            {
              id: 2,
              user_id: 10,
              user_email: 'ada@example.com',
              project_id: 2,
              issue_type: 'Project Request',
              entity_id: 'Rejected Idea',
              description: 'need it too',
              status: 'rejected',
              rejection_comment: 'Not now',
              created_at: '2026-01-02T00:00:00Z',
              updated_at: '2026-01-02T00:00:00Z',
            },
          ],
        }),
      ),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByText('Marketing Automation')).toBeInTheDocument();
    });
    expect(screen.getByText('Project #42 is ready')).toBeInTheDocument();
    expect(screen.getByText('Rejected Idea')).toBeInTheDocument();
    expect(screen.getByText('Reason: Not now')).toBeInTheDocument();
  });
});

describe('RequestProjectDialog — submit', () => {
  it('submits a request with the typed name and justification', async () => {
    let createdBody: { name: string; description: string } | undefined;
    server.use(
      http.get(MINE_PATH, () => HttpResponse.json({ total: 0, rows: [] })),
      http.post(CREATE_PATH, async ({ request }) => {
        createdBody = (await request.json()) as typeof createdBody;
        return HttpResponse.json(
          {
            id: 3,
            user_id: 10,
            user_email: 'ada@example.com',
            project_id: 2,
            issue_type: 'Project Request',
            entity_id: createdBody!.name,
            description: createdBody!.description,
            status: 'pending',
            rejection_comment: null,
            created_at: '2026-01-03T00:00:00Z',
            updated_at: '2026-01-03T00:00:00Z',
          },
          { status: 201 },
        );
      }),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByText("You haven't requested a project yet.")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByTestId('request-project-name').querySelector('input')!, {
      target: { value: 'Marketing Automation' },
    });
    fireEvent.change(screen.getByTestId('request-project-justification').querySelector('textarea')!, {
      target: { value: 'We need a project for the new campaign.' },
    });
    fireEvent.click(screen.getByTestId('request-project-submit'));

    await waitFor(() => {
      expect(createdBody).toEqual({ name: 'Marketing Automation', description: 'We need a project for the new campaign.' });
    });
    await waitFor(() => {
      expect(screen.getByText('Request submitted. An operator will review it.')).toBeInTheDocument();
    });
  });

  it('rejects an empty name or justification without calling the server', async () => {
    let createCalls = 0;
    server.use(
      http.get(MINE_PATH, () => HttpResponse.json({ total: 0, rows: [] })),
      http.post(CREATE_PATH, () => {
        createCalls += 1;
        return HttpResponse.json({}, { status: 201 });
      }),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByText("You haven't requested a project yet.")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByTestId('request-project-submit'));

    expect(await screen.findByText('Name the project')).toBeInTheDocument();
    expect(screen.getByText('Say why you need it')).toBeInTheDocument();
    expect(createCalls).toBe(0);
  });
});
