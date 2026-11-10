/**
 * Settings › Agent workforce (issue 1617): the policy is off by default,
 * only a manager edits it, a save sends the input shape, and each automatic
 * action offers exactly the decisions the server says are open.
 */
import { QueryClient } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AgentAutomaticAction, AgentWorkforcePolicy } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { formOf, inputOf, typeThreshold, withThreshold } from '../lib/policy';
import { WorkforcePolicyPanel } from './WorkforcePolicyPanel';

const BASE = '/api/v2';
const ROOT = `${BASE}/elitea_core/agent_workforce/prompt_lib/:projectId`;

const RULES = [
  { id: 'breaker_trips', conditions: [{ metric: 'breaker_trips_7d', op: '>=', value: 3 }], stage: 'suspend' },
  { id: 'unused', conditions: [{ metric: 'unused_days', op: '>=', value: 90 }], stage: 'retire' },
];

function policy(overrides: Partial<AgentWorkforcePolicy> = {}): AgentWorkforcePolicy {
  return {
    project_id: 7,
    saved: false,
    auto_hire_enabled: false,
    min_successful_runs: 3,
    max_auto_hired: 5,
    auto_dismiss_enabled: false,
    auto_dismiss_rules: RULES,
    grace_days: 7,
    updated_by: null,
    updated_at: null,
    default_rules: RULES,
    open_auto_hired: 0,
    can_manage: true,
    ...overrides,
  };
}

function action(overrides: Partial<AgentAutomaticAction> = {}): AgentAutomaticAction {
  return {
    id: 12,
    project_id: 7,
    application_id: 41,
    application_name: 'Triage',
    kind: 'auto_hire',
    rule: 'auto_hire',
    stage: null,
    proposal_id: 3,
    status: 'applied',
    due_at: '2026-11-23T10:00:00Z',
    evidence: {},
    evidence_digest: 'd',
    note: null,
    decided_by_user_id: null,
    decided_at: null,
    applied_at: '2026-11-09T10:00:00Z',
    created_at: '2026-11-09T10:00:00Z',
    can_undo: true,
    can_object: false,
    ...overrides,
  };
}

function mock(current: AgentWorkforcePolicy, actions: AgentAutomaticAction[] = []): { saved: unknown[]; decided: string[] } {
  const record = { saved: [] as unknown[], decided: [] as string[] };
  server.use(
    http.get(`${ROOT}/policy`, () => HttpResponse.json(current)),
    http.put(`${ROOT}/policy`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      record.saved.push(body);
      return HttpResponse.json({ ...current, ...body, saved: true });
    }),
    http.get(`${ROOT}/automatic_actions`, () => HttpResponse.json({ rows: actions, total: actions.length })),
    http.post(`${ROOT}/automatic_actions/:actionId/:decision`, ({ params }) => {
      record.decided.push(`${String(params['actionId'])}:${String(params['decision'])}`);
      if (params['decision'] === 'object') {
        return HttpResponse.json({ code: 'workforce_action_closed', message: 'The grace period is over.' }, { status: 409 });
      }
      return HttpResponse.json(action({ status: params['decision'] === 'undo' ? 'undone' : 'kept', can_undo: false }));
    }),
  );
  return record;
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('WorkforcePolicyPanel', () => {
  it('shows both policies off by default and saves what the manager turned on', async () => {
    const record = mock(policy());
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />);
    const autoHire = await screen.findByTestId('agent-workforce-policy-auto-hire');
    const hireInput = autoHire.querySelector('input') as HTMLInputElement;
    expect(hireInput.checked).toBe(false);
    expect((screen.getByTestId('agent-workforce-policy-auto-dismiss').querySelector('input') as HTMLInputElement).checked).toBe(false);
    expect(await screen.findByTestId('agent-workforce-actions-empty')).toBeInTheDocument();

    await userEvent.click(hireInput);
    const grace = screen.getByTestId('agent-workforce-policy-grace');
    await userEvent.clear(grace);
    await userEvent.type(grace, '14');
    await userEvent.click(screen.getByTestId('agent-workforce-policy-save'));
    await waitFor(() => expect(record.saved).toHaveLength(1));
    expect(record.saved[0]).toEqual({
      auto_hire_enabled: true,
      min_successful_runs: 3,
      max_auto_hired: 5,
      auto_dismiss_enabled: false,
      auto_dismiss_rules: RULES,
      grace_days: 14,
    });
  });

  it('does not save a cleared threshold as 0', async () => {
    const record = mock(policy());
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />);
    const unused = await screen.findByTestId('agent-workforce-rule-unused-0');
    await userEvent.clear(unused);
    expect((unused as HTMLInputElement).value).toBe('');
    expect(screen.getByTestId('agent-workforce-policy-save')).toBeDisabled();
    await userEvent.type(unused, '120');
    await userEvent.click(screen.getByTestId('agent-workforce-policy-save'));
    await waitFor(() => expect(record.saved).toHaveLength(1));
    expect((record.saved[0] as { auto_dismiss_rules: typeof RULES }).auto_dismiss_rules[1]?.conditions[0]?.value).toBe(120);
  });

  it('keeps unsaved edits when the policy refetches', async () => {
    mock(policy());
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />, queryClient);
    const grace = await screen.findByTestId('agent-workforce-policy-grace');
    await userEvent.clear(grace);
    await userEvent.type(grace, '21');
    await queryClient.refetchQueries();
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(screen.getByTestId('agent-workforce-policy-grace')).toHaveValue(21);
  });

  it('still offers Keep, but not Undo, once the undo window closed', async () => {
    const record = mock(policy(), [action({ can_undo: false })]);
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />);
    await userEvent.click(await screen.findByTestId('agent-workforce-action-keep-12'));
    await waitFor(() => expect(record.decided).toContain('12:keep'));
    expect(screen.queryByTestId('agent-workforce-action-undo-12')).not.toBeInTheDocument();
  });

  it('is read-only for a caller who is not an agent manager', async () => {
    mock(policy({ can_manage: false }), [action()]);
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />);
    const hire = await screen.findByTestId('agent-workforce-policy-auto-hire');
    expect((hire.querySelector('input') as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByTestId('agent-workforce-policy-save')).not.toBeInTheDocument();
    await screen.findByTestId('agent-workforce-action-12');
    expect(screen.queryByTestId('agent-workforce-action-undo-12')).not.toBeInTheDocument();
  });

  it('undoes an automatic hire and shows the server refusal of an objection', async () => {
    const record = mock(policy(), [action(), action({ id: 13, kind: 'auto_dismiss', rule: 'unused', stage: 'retire', status: 'pending', can_undo: false, can_object: true })]);
    renderWithQueryAndTheme(<WorkforcePolicyPanel projectId="7" />);
    await userEvent.click(await screen.findByTestId('agent-workforce-action-undo-12'));
    await waitFor(() => expect(record.decided).toContain('12:undo'));
    expect(screen.queryByTestId('agent-workforce-action-object-12')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-workforce-action-keep-13')).not.toBeInTheDocument();
    await userEvent.click(screen.getByTestId('agent-workforce-action-object-13'));
    expect(await screen.findByTestId('agent-workforce-action-error-13')).toHaveTextContent('The grace period is over.');
  });
});

describe('policy form helpers', () => {
  it('refuses to build an input while a number is out of bounds', () => {
    const form = formOf(policy());
    expect(inputOf(form)).toBeDefined();
    expect(inputOf({ ...form, graceDays: '0' })).toBeUndefined();
    expect(inputOf({ ...form, graceDays: '91' })).toBeUndefined();
    expect(inputOf({ ...form, maxAutoHired: '51' })).toBeUndefined();
    expect(inputOf({ ...form, minSuccessfulRuns: '2.5' })).toBeUndefined();
    expect(inputOf({ ...form, rules: withThreshold(form.rules, 0, 0, -1) })).toBeUndefined();
    expect(inputOf(typeThreshold(form, 1, 0, ''))).toBeUndefined();
    expect(inputOf(typeThreshold(form, 1, 0, '120'))?.auto_dismiss_rules[1]?.conditions[0]?.value).toBe(120);
  });

  it('changes one threshold and leaves the rest', () => {
    const rules = withThreshold(RULES, 1, 0, 120);
    expect(rules[1]?.conditions[0]?.value).toBe(120);
    expect(rules[0]).toBe(RULES[0]);
    expect(RULES[1]?.conditions[0]?.value).toBe(90);
  });
});
