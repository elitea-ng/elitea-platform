/**
 * Guards for Admin › Budgets (gap G4).
 *
 * The properties asserted here are the ones this page's specific hazards make
 * worth asserting. Every one of them is invisible to a status-code assertion:
 *
 *  1. **The dialog's validation refuses what the server refuses**, and refuses
 *     it locally so the operator gets the message beside the field. A negative
 *     ceiling and an out-of-range threshold are 400s; a BLANK limit is not, and
 *     it means "no ceiling" rather than zero.
 *  2. **An untouched fail mode is not sent as a clear.** `nats_fail_mode` has
 *     three inputs — absent, null, a mode — and a form that always sent the
 *     field would silently reset a policy on every limit edit. The recorded
 *     body is checked for the KEY, not just for its value.
 *  3. **The member payload carries neither project-scoped field.** The server
 *     answers 400 for either, so sending one turns a valid member edit into a
 *     refusal.
 *  4. **The enforcement banner warns only on a positive report.** An
 *     unreachable gateway, and a gateway too old to carry the field, must both
 *     render nothing — warning on silence turns a network fault into a claim
 *     about policy.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminBudgetDialog } from './AdminBudgetDialog';
import { AdminBudgets } from './Budgets';
import type { BudgetFormValues } from './api/adminBudgetsApi';
import { renderAdminRoute } from './__tests__/testRouter';

/** One team project with an authored, enforced ceiling. */
const PROJECT_ROW = {
  project_id: 21,
  name: 'alpha-team',
  display_name: 'alpha-team',
  owner_name: 'Ada',
  owner_email: 'ada@example.com',
  is_personal: false,
  monthly_limit: 100,
  effective_limit: 100,
  limit_source: 'explicit',
  currency: 'USD',
  enabled: true,
  warning_pct: 80,
  spend: 12.5,
  remaining: 87.5,
  percent_used: 12.5,
  spend_available: true,
  period: '202608',
};

/** The per-project read the dialog performs, carrying the two policy columns. */
const PROJECT_BUDGET = {
  ...PROJECT_ROW,
  budget_period: 'monthly',
  nats_fail_mode: 'fail_open',
};

const MEMBER_ROW = {
  project_id: 21,
  user_id: 502,
  name: 'Bo',
  email: 'bo@example.com',
  roles: ['editor'],
  enforced: true,
  monthly_limit: 25,
  effective_limit: 25,
  limit_source: 'explicit',
  currency: 'USD',
  enabled: true,
  warning_pct: 80,
  spend: 1,
  remaining: 24,
  percent_used: 4,
  spend_available: true,
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

/** The gateway status body. `undefined` omits the field entirely. */
interface StatusOptions {
  readonly reachable?: boolean;
  readonly rateLimitsEnforceable?: boolean | undefined;
}

function useBudgetHandlers(status: StatusOptions = {}): void {
  server.use(
    http.get('*/elitea_core/project_budgets/administration*', () =>
      HttpResponse.json({ rows: [PROJECT_ROW], total: 1, counts: { team: 1, personal: 0 } }),
    ),
    http.get('*/elitea_core/project_budget/administration/*', () =>
      HttpResponse.json(PROJECT_BUDGET),
    ),
    http.put('*/elitea_core/project_budget/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json(PROJECT_BUDGET);
    }),
    http.delete('*/elitea_core/project_budget/administration/*', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return HttpResponse.json({ ...PROJECT_BUDGET, monthly_limit: null });
    }),
    http.get('*/elitea_core/user_budgets/administration/*', () =>
      HttpResponse.json({ rows: [MEMBER_ROW], total: 1, warning_pct: 80 }),
    ),
    http.put('*/elitea_core/user_budget/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json(MEMBER_ROW);
    }),
    http.get('*/admin/gateway/status', () => {
      const gateway: Record<string, unknown> = {};
      if (status.rateLimitsEnforceable !== undefined) {
        gateway['rate_limits_enforceable'] = status.rateLimitsEnforceable;
      }
      return HttpResponse.json({ reachable: status.reachable ?? true, gateway });
    }),
  );
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

/** Opens the project edit dialog and waits for the per-project read to land. */
async function openProjectDialog(): Promise<void> {
  await screen.findByText('alpha-team');
  await userEvent.click(await screen.findByTestId('admin-budgets-edit-21'));
  // The dialog opens BEFORE the policy read resolves. Waiting for the seeded
  // value is what proves the form is not still showing its blanks.
  await waitFor(() => {
    expect(screen.getByTestId('admin-budget-limit')).toHaveValue('100');
  });
}

function lastBody(): Record<string, unknown> {
  const write = recorded.at(-1);
  expect(write).toBeDefined();
  return write?.body as Record<string, unknown>;
}

/**
 * The validation cases render the DIALOG alone rather than the page.
 *
 * They are properties of the form, and mounting a DataGrid and four queries to
 * reach it made each case spend most of its time on machinery it does not
 * assert about — which is also how they came to sit on the edge of the default
 * timeout. The payload-shape cases below still go through the whole page,
 * because "what reaches the wire" is exactly the part a dialog rendered in
 * isolation cannot answer.
 */
describe('the edit dialog validation', () => {
  const PROJECT_SEED = {
    monthlyLimit: 100,
    enabled: true,
    softAlertPct: 80,
    budgetPeriod: 'monthly',
    natsFailMode: 'fail_open',
  };

  function renderDialog(): { readonly submitted: BudgetFormValues[] } {
    const submitted: BudgetFormValues[] = [];
    renderAdminRoute(
      <AdminBudgetDialog
        open
        scope="project"
        subjectName="alpha-team"
        initial={PROJECT_SEED}
        isLoading={false}
        isSaving={false}
        serverError={undefined}
        onClose={() => {}}
        onSubmit={(values) => submitted.push(values)}
      />,
    );
    return { submitted };
  }

  it('refuses a negative ceiling without submitting', async () => {
    const { submitted } = renderDialog();

    await userEvent.clear(screen.getByTestId('admin-budget-limit'));
    await userEvent.type(screen.getByTestId('admin-budget-limit'), '-5');
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    expect(await screen.findByTestId('admin-budget-dialog-error')).toHaveTextContent(/negative/i);
    expect(submitted).toHaveLength(0);
  });

  it('refuses a non-numeric ceiling without submitting', async () => {
    const { submitted } = renderDialog();

    await userEvent.clear(screen.getByTestId('admin-budget-limit'));
    await userEvent.type(screen.getByTestId('admin-budget-limit'), 'ten');
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    expect(await screen.findByTestId('admin-budget-dialog-error')).toHaveTextContent(/number/i);
    expect(submitted).toHaveLength(0);
  });

  it('refuses a threshold outside 1..100 without submitting', async () => {
    const { submitted } = renderDialog();

    await userEvent.clear(screen.getByTestId('admin-budget-threshold'));
    await userEvent.type(screen.getByTestId('admin-budget-threshold'), '101');
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    expect(await screen.findByTestId('admin-budget-dialog-error')).toHaveTextContent(/1 to 100/i);
    expect(submitted).toHaveLength(0);
  });

  // A blank limit is NOT an error. It means "no ceiling", which is a different
  // and legitimate state from the zero a required field would force.
  it('accepts a blank ceiling as "no ceiling" rather than refusing it', async () => {
    const { submitted } = renderDialog();

    await userEvent.clear(screen.getByTestId('admin-budget-limit'));
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0]?.monthlyLimit).toBeNull();
  });

  // A blank threshold means "inherit the platform value", which the server
  // reads from an OMITTED field — so it must not become a number here.
  it('accepts a blank threshold as "inherit"', async () => {
    const { submitted } = renderDialog();

    await userEvent.clear(screen.getByTestId('admin-budget-threshold'));
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0]?.softAlertPct).toBeNull();
  });
});

describe('what reaches the wire', () => {
  // The tri-state. The dialog seeds the stored mode and sends it back
  // unchanged; it must never send `null` for a field the operator did not
  // touch, because `null` is the instruction to clear the override.
  it('round-trips the stored fail mode instead of clearing it', async () => {
    useBudgetHandlers();
    renderAdminRoute(<AdminBudgets />);
    await openProjectDialog();

    await userEvent.click(screen.getByTestId('admin-budget-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    const body = lastBody();
    expect(body['nats_fail_mode']).toBe('fail_open');
    expect(body['budget_period']).toBe('monthly');
    expect(body['monthly_limit']).toBe(100);
  });

  // The server answers 400 for either field at member scope, so the payload
  // must not carry the KEY at all — an explicit `undefined` would still
  // serialise away, but a `null` would not.
  it('omits both project-scoped fields from a member write', async () => {
    useBudgetHandlers();
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    await userEvent.click(await screen.findByTestId('admin-budgets-members-21'));
    await userEvent.click(await screen.findByTestId('admin-budgets-member-edit-502'));

    await waitFor(() => {
      expect(screen.getByTestId('admin-budget-limit')).toHaveValue('25');
    });
    await userEvent.clear(screen.getByTestId('admin-budget-limit'));
    await userEvent.type(screen.getByTestId('admin-budget-limit'), '40');
    await userEvent.click(screen.getByTestId('admin-budget-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    const body = lastBody();
    expect(body).not.toHaveProperty('nats_fail_mode');
    expect(body).not.toHaveProperty('budget_period');
    expect(body['monthly_limit']).toBe(40);
  });

  // The policy fields do not exist at member scope, so the controls that write
  // them must not be on screen either.
  it('hides the policy controls in the member dialog', async () => {
    useBudgetHandlers();
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    await userEvent.click(await screen.findByTestId('admin-budgets-members-21'));
    await userEvent.click(await screen.findByTestId('admin-budgets-member-edit-502'));

    await screen.findByTestId('admin-budget-limit');
    expect(screen.queryByTestId('admin-budget-fail-mode')).toBeNull();
    expect(screen.queryByTestId('admin-budget-period')).toBeNull();
  });
});

describe('the enforcement warning (G12)', () => {
  it('warns when a reachable gateway reports it cannot enforce', async () => {
    useBudgetHandlers({ reachable: true, rateLimitsEnforceable: false });
    renderAdminRoute(<AdminBudgets />);

    expect(await screen.findByTestId('admin-budgets-enforcement-off')).toHaveTextContent(
      /GATEWAY_NATS_URL/,
    );
  });

  it('stays silent when the gateway is enforcing', async () => {
    useBudgetHandlers({ reachable: true, rateLimitsEnforceable: true });
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    expect(screen.queryByTestId('admin-budgets-enforcement-off')).toBeNull();
  });

  // An unreachable hop is not evidence about policy. Warning here would turn a
  // transient network fault into "your budgets do nothing".
  it('stays silent when the gateway did not answer', async () => {
    useBudgetHandlers({ reachable: false, rateLimitsEnforceable: false });
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    expect(screen.queryByTestId('admin-budgets-enforcement-off')).toBeNull();
  });

  // Absent is not false — the same rule `SharedScopeWarning` applies to
  // `shared_project_id`.
  it('stays silent when the gateway is too old to report the field', async () => {
    useBudgetHandlers({ reachable: true, rateLimitsEnforceable: undefined });
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    expect(screen.queryByTestId('admin-budgets-enforcement-off')).toBeNull();
  });
});

describe('the clear action', () => {
  it('sends a DELETE rather than an enabled:false write', async () => {
    useBudgetHandlers();
    renderAdminRoute(<AdminBudgets />);

    await screen.findByText('alpha-team');
    await userEvent.click(await screen.findByTestId('admin-budgets-clear-21'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]?.method).toBe('DELETE');
    expect(recorded[0]?.url).toContain('/project_budget/administration/21/budget');
  });
});
