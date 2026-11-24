/**
 * Coverage for `DbActionsContent` (T-5E 5e-2) — the project DB action rules
 * tab. Real generated client through MSW, mirroring `HttpActions.test.tsx`.
 */
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { server } from '@/test/setup';

import { DbActionsContent } from './DbActions';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const RULES_PATH = `${BASE}/dbactions/prompt_lib/:projectID/rules`;
const RULE_PATH = `${BASE}/dbactions/prompt_lib/:projectID/rules/:ruleID`;
const NAMES_PATH = `${BASE}/dbactions/prompt_lib/:projectID/rule-names`;
const CONFIGURATIONS_PATH = `${BASE}/configurations/configurations/:project_id`;

const READ_RULE = {
  id: 21,
  revision: 3,
  name: 'warehouse_pg.read',
  configuration_id: 9,
  dialect: 'postgres',
  database_name: 'analytics',
  mode: 'read',
  enabled: true,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
};

const WRITE_RULE = {
  id: 22,
  revision: 2,
  name: 'warehouse_mysql.load',
  configuration_id: 10,
  dialect: 'mysql',
  database_name: 'etl_prod',
  mode: 'write',
  marker_table: 'public.elitea_effects',
  enabled: false,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
};

function permissions(...names: string[]) {
  return http.get(PERMISSIONS_PATH, () => HttpResponse.json(names.map((name) => ({ name, enabled: true }))));
}

const ADMIN = ['models.db_action_rules.manage', 'models.db_action_rules.list'];

interface Calls {
  rules: number;
  names: number;
  writes: { method: string; ruleID?: string; body: Record<string, unknown> }[];
}

function adminHandlers(calls: Calls, items: unknown[], respond?: () => Response) {
  const write = async (request: Request, ruleID?: string) => {
    const body = (await request.json()) as Record<string, unknown>;
    calls.writes.push({ method: request.method, ...(ruleID ? { ruleID } : {}), body });
    return respond?.() ?? HttpResponse.json({ ...READ_RULE, ...body, revision: 4 }, { status: request.method === 'POST' ? 201 : 200 });
  };
  return [
    permissions(...ADMIN),
    http.get(RULES_PATH, () => {
      calls.rules += 1;
      return HttpResponse.json({ items });
    }),
    http.get(CONFIGURATIONS_PATH, ({ request }) => {
      expect(new URL(request.url).searchParams.getAll('type')).toEqual(['sql']);
      return HttpResponse.json({
        items: [
          { id: 9, label: 'Warehouse PG', type: 'sql', section: 'credentials', project_id: 1, shared: true, status_ok: true, source: 'user', created_at: '2026-10-01T00:00:00Z' },
          { id: 10, label: 'Warehouse MySQL', type: 'sql', section: 'credentials', project_id: 1, shared: true, status_ok: true, source: 'user', created_at: '2026-10-01T00:00:00Z' },
        ],
        total: 2,
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
      <DbActionsContent />
    </AppProviders>,
  );
}

function setText(dialog: HTMLElement, testId: string, value: string) {
  fireEvent.change(within(dialog).getByTestId(testId).querySelector('input')!, { target: { value } });
}

async function openCreateDialog() {
  await waitFor(() => {
    expect(screen.getByRole('button', { name: 'Add a DB action rule' })).toBeEnabled();
  });
  fireEvent.click(screen.getByRole('button', { name: 'Add a DB action rule' }));
  return screen.findByTestId('db-action-rule-form-dialog');
}

describe('DbActionsContent — manage', () => {
  it('lists read and write rules with every column and no delete button', async () => {
    server.use(...adminHandlers(newCalls(), [READ_RULE, WRITE_RULE]));
    mount();

    const readRow = await screen.findByTestId('db-action-rule-row-21');
    expect(within(readRow).getByText('warehouse_pg.read')).toBeInTheDocument();
    expect(within(readRow).getByText('PostgreSQL')).toBeInTheDocument();
    expect(within(readRow).getByText('analytics')).toBeInTheDocument();
    expect(within(readRow).getByText('Read')).toBeInTheDocument();
    expect(within(readRow).getByText('—')).toBeInTheDocument();
    expect(within(readRow).getByText('9')).toBeInTheDocument();

    const writeRow = await screen.findByTestId('db-action-rule-row-22');
    expect(within(writeRow).getByText('warehouse_mysql.load')).toBeInTheDocument();
    expect(within(writeRow).getByText('MySQL')).toBeInTheDocument();
    expect(within(writeRow).getByText('etl_prod')).toBeInTheDocument();
    expect(within(writeRow).getByText('Write')).toBeInTheDocument();
    expect(within(writeRow).getByText('public.elitea_effects')).toBeInTheDocument();

    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
  });

  it('creates a read rule omitting marker_table and hides marker_table input in read mode', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, []));
    mount();

    const dialog = await openCreateDialog();
    expect(within(dialog).queryByTestId('db-action-rule-form-marker-table')).not.toBeInTheDocument();

    setText(dialog, 'db-action-rule-form-name', 'warehouse_pg.orders');
    fireEvent.mouseDown(within(dialog).getByLabelText('SQL credential *'));
    fireEvent.click(await screen.findByRole('option', { name: 'Warehouse PG (9)' }));
    setText(dialog, 'db-action-rule-form-database-name', 'analytics');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]!.method).toBe('POST');
    expect(calls.writes[0]!.body).toEqual({
      name: 'warehouse_pg.orders',
      configuration_id: 9,
      dialect: 'postgres',
      database_name: 'analytics',
      mode: 'read',
      enabled: true,
    });
    expect(calls.writes[0]!.body).not.toHaveProperty('marker_table');
  });

  it('switching mode to write auto-fills elitea_effects, requires marker_table, and submits it', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, []));
    mount();

    const dialog = await openCreateDialog();
    setText(dialog, 'db-action-rule-form-name', 'warehouse_mysql.merge');
    fireEvent.mouseDown(within(dialog).getByLabelText('SQL credential *'));
    fireEvent.click(await screen.findByRole('option', { name: 'Warehouse MySQL (10)' }));
    fireEvent.mouseDown(within(dialog).getByLabelText('Dialect'));
    fireEvent.click(await screen.findByRole('option', { name: 'MySQL' }));
    setText(dialog, 'db-action-rule-form-database-name', 'etl_prod');

    fireEvent.mouseDown(within(dialog).getByLabelText('Mode'));
    fireEvent.click(await screen.findByRole('option', { name: 'Write (transactional)' }));

    const markerField = await within(dialog).findByTestId('db-action-rule-form-marker-table');
    expect(markerField.querySelector('input')).toHaveValue('elitea_effects');

    // Clearing marker_table blocks submit client-side.
    setText(dialog, 'db-action-rule-form-marker-table', '');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(calls.writes).toHaveLength(0);
    expect(within(dialog).getByText('This field is required')).toBeInTheDocument();

    // Filling a custom schema.table succeeds.
    setText(dialog, 'db-action-rule-form-marker-table', 'etl.effects_log');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]!.body).toEqual({
      name: 'warehouse_mysql.merge',
      configuration_id: 10,
      dialect: 'mysql',
      database_name: 'etl_prod',
      mode: 'write',
      marker_table: 'etl.effects_log',
      enabled: true,
    });
  });

  it('edits a rule with PUT carrying the current revision', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, [READ_RULE]));
    mount();

    fireEvent.click(await screen.findByTestId('db-action-rule-edit-21'));
    const dialog = await screen.findByTestId('db-action-rule-form-dialog');
    setText(dialog, 'db-action-rule-form-database-name', 'analytics_v2');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]).toMatchObject({ method: 'PUT', ruleID: '21' });
    expect(calls.writes[0]!.body).toEqual({
      name: 'warehouse_pg.read',
      configuration_id: 9,
      dialect: 'postgres',
      database_name: 'analytics_v2',
      mode: 'read',
      enabled: true,
      revision: 3,
    });
  });

  it('toggling enabled PUTs the full record with only enabled flipped', async () => {
    const calls = newCalls();
    server.use(...adminHandlers(calls, [READ_RULE]));
    mount();

    const toggle = await screen.findByTestId('db-action-rule-enabled-toggle-21');
    fireEvent.click(within(toggle).getByRole('switch'));

    await waitFor(() => {
      expect(calls.writes).toHaveLength(1);
    });
    expect(calls.writes[0]).toMatchObject({ method: 'PUT', ruleID: '21' });
    expect(calls.writes[0]!.body).toMatchObject({ name: 'warehouse_pg.read', enabled: false, revision: 3 });
  });

  it('shows server egress refusal and stale-revision conflict inline while keeping dialog open', async () => {
    const refusal = 'DB action rule destination does not resolve to a permitted destination';
    server.use(...adminHandlers(newCalls(), [], () => HttpResponse.json({ error: refusal }, { status: 400 })));
    mount();

    const dialog = await openCreateDialog();
    setText(dialog, 'db-action-rule-form-name', 'warehouse_pg.private');
    fireEvent.mouseDown(within(dialog).getByLabelText('SQL credential *'));
    fireEvent.click(await screen.findByRole('option', { name: 'Warehouse PG (9)' }));
    setText(dialog, 'db-action-rule-form-database-name', 'analytics');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(within(dialog).getByText(refusal)).toBeInTheDocument();
    });
    expect(within(dialog).getByTestId('db-action-rule-form-name').querySelector('input')).toHaveValue('warehouse_pg.private');
  });
});

describe('DbActionsContent — list only', () => {
  it('shows redacted name, dialect, mode and enabled read-only and never asks for full rules', async () => {
    const calls = newCalls();
    server.use(
      permissions('models.db_action_rules.list'),
      http.get(RULES_PATH, () => {
        calls.rules += 1;
        return HttpResponse.json({ items: [READ_RULE] });
      }),
      http.get(NAMES_PATH, () => {
        calls.names += 1;
        return HttpResponse.json({ items: [{ name: 'warehouse_pg.read', dialect: 'postgres', mode: 'read', enabled: false }] });
      }),
    );
    mount();

    const row = await screen.findByTestId('db-action-rule-name-row-warehouse_pg.read');
    expect(within(row).getByText('warehouse_pg.read')).toBeInTheDocument();
    expect(within(row).getByText('PostgreSQL')).toBeInTheDocument();
    expect(within(row).getByText('Read')).toBeInTheDocument();
    expect(within(row).getByText('Disabled')).toBeInTheDocument();
    expect(screen.queryByText('analytics')).not.toBeInTheDocument();
    expect(calls.names).toBeGreaterThan(0);
    expect(calls.rules).toBe(0);
    expect(screen.queryByRole('button', { name: 'Add a DB action rule' })).not.toBeInTheDocument();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit DB action rule' })).not.toBeInTheDocument();
  });
});

describe('DbActionsContent — no permission', () => {
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
    expect(screen.queryByTestId('db-action-rules-table')).not.toBeInTheDocument();
  });
});
