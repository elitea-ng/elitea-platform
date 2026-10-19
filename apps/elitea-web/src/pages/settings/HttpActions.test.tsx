/**
 * Coverage for `HttpActionsContent` (T-5D) — the project HTTP action rules
 * tab. Real generated client through MSW, the same harness as
 * `Webhooks.test.tsx`.
 */
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { server } from '@/test/setup';

import { HttpActionsContent } from './HttpActions';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const RULES_PATH = `${BASE}/httpactions/prompt_lib/:projectID/rules`;
const RULE_PATH = `${BASE}/httpactions/prompt_lib/:projectID/rules/:ruleID`;
const NAMES_PATH = `${BASE}/httpactions/prompt_lib/:projectID/rule-names`;
const CONFIGURATIONS_PATH = `${BASE}/configurations/configurations/:project_id`;

const RULE = {
  id: 11,
  revision: 3,
  name: 'crm.lookup',
  origin: 'https://crm.example.com',
  path_template: '/customers/{customer_id}',
  method: 'GET',
  query_names: ['expand'],
  configuration_id: 0,
  toolkit: 'crm',
  tool: 'lookup',
  replay: 'never',
  enabled: true,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
};

function permissions(...names: string[]) {
  return http.get(PERMISSIONS_PATH, () => HttpResponse.json(names.map((name) => ({ name, enabled: true }))));
}

const ADMIN = ['models.http_action_rules.manage', 'models.http_action_rules.list'];

interface Calls {
  rules: number;
  names: number;
  writes: { method: string; ruleID?: string; body: Record<string, unknown> }[];
}

/** Admin handlers; `respond` overrides the write answer (default: echo the body as the stored rule). */
function adminHandlers(calls: Calls, items: unknown[], respond?: () => Response) {
  const write = async (request: Request, ruleID?: string) => {
    const body = (await request.json()) as Record<string, unknown>;
    calls.writes.push({ method: request.method, ...(ruleID ? { ruleID } : {}), body });
    return respond?.() ?? HttpResponse.json({ ...RULE, ...body, revision: 4 }, { status: request.method === 'POST' ? 201 : 200 });
  };
  return [
    permissions(...ADMIN),
    http.get(RULES_PATH, () => {
      calls.rules += 1;
      return HttpResponse.json({ items });
    }),
    http.get(CONFIGURATIONS_PATH, ({ request }) => {
      expect(new URL(request.url).searchParams.getAll('type')).toEqual(['openapi']);
      return HttpResponse.json({
        items: [{ id: 7, label: 'CRM key', type: 'openapi', section: 'credentials', project_id: 1, shared: false, status_ok: true, source: 'user', created_at: '2026-10-01T00:00:00Z' }],
        total: 1,
        offset: 0,
        limit: 200,
      });
    }),
    http.post(RULES_PATH, ({ request }) => write(request)),
    http.put(RULE_PATH, ({ request, params }) => write(request, String(params.ruleID))),
  ];
}

function newCalls(): Calls {
  return { rules: 0, names: 0, writes: [] };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
});

afterEach(() => {
  resetGeneratedClient();
  useSelectedProjectStore.setState({ project: null });
});

function mount() {
  return render(
    <AppProviders>
      <HttpActionsContent />
    </AppProviders>,
  );
}

function setText(dialog: HTMLElement, testId: string, value: string) {
  fireEvent.change(within(dialog).getByTestId(testId).querySelector('input')!, { target: { value } });
}

async function openCreateForm() {
  await waitFor(() => {
    expect(screen.getByRole('button', { name: 'Add an HTTP action rule' })).toBeEnabled();
  });
  fireEvent.click(screen.getByRole('button', { name: 'Add an HTTP action rule' }));
  const dialog = await screen.findByTestId('http-action-rule-form-dialog');
  setText(dialog, 'http-action-rule-form-name', 'billing.charge');
  setText(dialog, 'http-action-rule-form-origin', 'https://billing.example.com');
  setText(dialog, 'http-action-rule-form-path-template', '/charges/{charge_id}');
  setText(dialog, 'http-action-rule-form-toolkit', 'billing');
  setText(dialog, 'http-action-rule-form-tool', 'charge');
  return dialog;
}

describe('HttpActionsContent — manage', () => {
  it('lists the rules with every column', async () => {
    server.use(...adminHandlers(newCalls(), [RULE]));
    mount();

    const row = await screen.findByTestId('http-action-rule-row-11');
    expect(within(row).getByText('crm.lookup')).toBeInTheDocument();
    expect(within(row).getByText('GET')).toBeInTheDocument();
    expect(within(row).getByText('https://crm.example.com/customers/{customer_id}')).toBeInTheDocument();
    expect(within(row).getByText('None')).toBeInTheDocument();
    expect(within(row).getByText('Never')).toBeInTheDocument();
  });

  it('creates a rule with the full write body', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, []));
    mount();

    const dialog = await openCreateForm();
    const queryInput = within(dialog).getByTestId('http-action-rule-form-query-names').querySelector('input')!;
    fireEvent.change(queryInput, { target: { value: 'currency' } });
    fireEvent.keyDown(queryInput, { key: 'Enter' });
    fireEvent.mouseDown(within(dialog).getByLabelText('Method'));
    fireEvent.click(await screen.findByRole('option', { name: 'POST' }));
    fireEvent.mouseDown(within(dialog).getByLabelText('Credential'));
    fireEvent.click(await screen.findByRole('option', { name: 'CRM key (7)' }));
    fireEvent.mouseDown(within(dialog).getByLabelText('Replay'));
    fireEvent.click(await screen.findByRole('option', { name: 'Idempotency key' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]!.method).toBe('POST');
    expect(calls.writes[0]!.body).toEqual({
      name: 'billing.charge',
      origin: 'https://billing.example.com',
      path_template: '/charges/{charge_id}',
      method: 'POST',
      query_names: ['currency'],
      configuration_id: 7,
      toolkit: 'billing',
      tool: 'charge',
      replay: 'idempotency_key',
      enabled: true,
    });
    await waitFor(() => {
      expect(screen.queryByTestId('http-action-rule-form-dialog')).not.toBeInTheDocument();
    });
  });

  it('edits a rule with a PUT that carries the current revision', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, [RULE]));
    mount();

    fireEvent.click(await screen.findByTestId('http-action-rule-edit-11'));
    const dialog = await screen.findByTestId('http-action-rule-form-dialog');
    expect(within(dialog).getByTestId('http-action-rule-form-name').querySelector('input')).toHaveValue('crm.lookup');
    setText(dialog, 'http-action-rule-form-tool', 'lookup_v2');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]).toMatchObject({ method: 'PUT', ruleID: '11' });
    expect(calls.writes[0]!.body).toEqual({
      name: 'crm.lookup',
      origin: 'https://crm.example.com',
      path_template: '/customers/{customer_id}',
      method: 'GET',
      query_names: ['expand'],
      configuration_id: 0,
      toolkit: 'crm',
      tool: 'lookup_v2',
      replay: 'never',
      enabled: true,
      revision: 3,
    });
  });

  it('toggling enabled PUTs the full record with only `enabled` flipped', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, [RULE]));
    mount();

    const toggle = await screen.findByTestId('http-action-rule-enabled-toggle-11');
    fireEvent.click(within(toggle).getByRole('switch'));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]).toMatchObject({ method: 'PUT', ruleID: '11' });
    expect(calls.writes[0]!.body).toMatchObject({ name: 'crm.lookup', tool: 'lookup', enabled: false, revision: 3 });
  });

  it('shows the server egress refusal inline and keeps the dialog open', async () => {
    const refusal = 'HTTP action rule origin refused: "10.0.0.5" is not a permitted destination';
    server.use(...adminHandlers(newCalls(), [], () => HttpResponse.json({ error: refusal }, { status: 400 })));
    mount();

    const dialog = await openCreateForm();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(within(dialog).getByText(refusal)).toBeInTheDocument();
    });
    expect(within(dialog).getByTestId('http-action-rule-form-name').querySelector('input')).toHaveValue('billing.charge');
  });

  it('shows the stale-revision conflict inline', async () => {
    const conflict = 'HTTP action rule revision is stale; reload and try again';
    server.use(...adminHandlers(newCalls(), [RULE], () => HttpResponse.json({ error: conflict }, { status: 409 })));
    mount();

    fireEvent.click(await screen.findByTestId('http-action-rule-edit-11'));
    const dialog = await screen.findByTestId('http-action-rule-form-dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(within(dialog).getByText(conflict)).toBeInTheDocument();
    });
  });
});

describe('HttpActionsContent — list only', () => {
  it('shows names and enabled state read-only and never asks for full rules', async () => {
    const calls = newCalls();
    server.use(
      permissions('models.http_action_rules.list'),
      http.get(RULES_PATH, () => {
        calls.rules += 1;
        return HttpResponse.json({ items: [RULE] });
      }),
      http.get(NAMES_PATH, () => {
        calls.names += 1;
        return HttpResponse.json({ items: [{ name: 'crm.lookup', enabled: false }] });
      }),
    );
    mount();

    expect(await screen.findByText('crm.lookup')).toBeInTheDocument();
    expect(screen.getByText('Disabled')).toBeInTheDocument();
    expect(calls.names).toBeGreaterThan(0);
    expect(calls.rules).toBe(0);
    expect(screen.queryByRole('button', { name: 'Add an HTTP action rule' })).not.toBeInTheDocument();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit HTTP action rule' })).not.toBeInTheDocument();
  });
});

describe('HttpActionsContent — no permission', () => {
  it('shows the no-access banner and requests no rules', async () => {
    const calls = newCalls();
    server.use(
      permissions(),
      http.get(RULES_PATH, () => {
        calls.rules += 1;
        return HttpResponse.json({ items: [] });
      }),
      http.get(NAMES_PATH, () => {
        calls.names += 1;
        return HttpResponse.json({ items: [] });
      }),
    );
    mount();

    expect(await screen.findByText('You do not have permission to view this setting.')).toBeInTheDocument();
    expect(calls.rules).toBe(0);
    expect(calls.names).toBe(0);
    expect(screen.queryByTestId('http-action-rules-table')).not.toBeInTheDocument();
  });
});
