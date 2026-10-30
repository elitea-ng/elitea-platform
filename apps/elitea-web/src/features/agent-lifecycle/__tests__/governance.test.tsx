/**
 * Agent lifecycle governance (ADR-0040, #1497): the tier-aware Publish
 * (Request publish), the 409 `change_request_required` fallback, the
 * governance dialog's approve and its refusals, and the lapsed banner. Every
 * test asserts the request that reached the server or the sentence the
 * server's answer produced.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AgentChangeRequest, AgentRegistryRecord } from '@/shared/api/generated/model';
import { EliteaApiError, configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { EntityLifecycleControls } from '../ui/EntityLifecycleControls';
import { GovernanceDialog } from '../ui/GovernanceDialog';
import { renderWithProviders } from './testUtils';

const BASE = '/api/v2';
const PROJECT = '2';
const CORE = `${BASE}/elitea_core`;

const REGISTRY: AgentRegistryRecord = {
  project_id: 2,
  application_id: 7,
  application_name: 'Audit Agent',
  business_owner_id: 4,
  technical_owner_id: 4,
  risk_tier: 'medium',
  intended_use: 'Answers audit questions.',
  out_of_scope_use: '',
  data_classes: ['customer_pii'],
  certification_state: 'uncertified',
  certified_release_id: null,
  certified_at: null,
  recertify_by: null,
  state_reason: '',
  state_changed_at: '2026-10-29T09:00:00Z',
  suspension_id: null,
  circuit_breaker: {},
  updated_at: '2026-10-29T09:00:00Z',
};

const CHANGE: AgentChangeRequest = {
  id: 41,
  application_id: 7,
  kind: 'release',
  state: 'submitted',
  risk_tier: 'medium',
  candidate_version_id: 11,
  candidate_revision_id: 3,
  version_name: 'v2',
  category: 'Development',
  summary: 'Adds the refund policy.',
  proposed_by: 4,
  self_approved: false,
  required_approvals: 1,
  required_governance_approvals: 0,
  gate_id: 12,
  gate: { id: 12, state: 'failed', failures: ['gate_regression'], overridden: false },
  override_reason: '',
  overridden_by: null,
  overridden_at: null,
  current_release_id: 5,
  release_id: null,
  decided_by: null,
  decided_at: null,
  decision_reason: '',
  created_at: '2026-10-29T09:00:00Z',
  submitted_at: '2026-10-29T09:01:00Z',
  published_at: null,
  approvals: [],
};

let registry: AgentRegistryRecord;
let registryReads: number;
let calls: { url: string; body: unknown }[];

function refusal(code: string, status: number, extra: Record<string, unknown> = {}) {
  return HttpResponse.json({ error: code, code, message: `server text for ${code}`, ...extra }, { status });
}

beforeEach(() => {
  registry = { ...REGISTRY };
  registryReads = 0;
  calls = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${CORE}/agent_registry/prompt_lib/:p/:a`, () => {
      registryReads += 1;
      return HttpResponse.json(registry);
    }),
    http.post(`${CORE}/change_requests/prompt_lib/:p/:a`, async ({ request }) => {
      calls.push({ url: request.url, body: await request.json() });
      return HttpResponse.json({ ...CHANGE, state: 'draft', gate_id: null, gate: undefined }, { status: 201 });
    }),
    http.post(`${CORE}/change_requests/prompt_lib/:p/:a/:cr/submit`, ({ request }) => {
      calls.push({ url: request.url, body: null });
      return HttpResponse.json(CHANGE);
    }),
    http.get(`${CORE}/change_requests/prompt_lib/:p/:a`, () => HttpResponse.json({ total: 1, rows: [CHANGE] })),
    http.get(`${CORE}/change_requests/prompt_lib/:p/:a/:cr`, () => HttpResponse.json(CHANGE)),
    http.get(`${CORE}/release_gates/prompt_lib/:p/:a/:gate`, () =>
      HttpResponse.json({
        id: 12,
        application_id: 7,
        kind: 'change',
        candidate_version_id: 11,
        current_release_id: 5,
        baseline_version_id: 9,
        state: 'failed',
        suites: [],
        runs: [],
        outcome: [
          {
            dataset_id: 3,
            regression: true,
            criteria: [],
            regressed_cases: [{ case_id: 'c1', name: 'Refund after 30 days', dimension_ids: ['5'] }],
          },
        ],
        failures: [{ code: 'gate_regression', message: '1 case regressed on the regression suite.' }],
        created_by: 4,
        created_at: '2026-10-29T09:01:00Z',
        evaluated_at: '2026-10-29T09:05:00Z',
        override: null,
        blocks: true,
      }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function controls() {
  return (
    <EntityLifecycleControls
      entity="agents"
      projectId={PROJECT}
      projects={[{ id: '2', name: 'Team' }]}
      entityId="7"
      entityName="Audit Agent"
      tab="all"
      activeVersionId="11"
      activeVersionStatus="draft"
    />
  );
}

/** The tier decides what Publish opens, so the tests wait for its read to settle. */
async function registryRead(): Promise<void> {
  await waitFor(() => expect(registryReads).toBeGreaterThan(0));
  await new Promise((resolve) => setTimeout(resolve, 20));
}

async function fillRequest(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  const dialog = await screen.findByTestId('request-publish-dialog');
  await user.type(within(dialog).getByTestId('request-publish-summary'), 'Adds the refund policy.');
  await user.type(within(dialog).getByTestId('publish-version-name'), 'v2');
  await user.click(within(dialog).getByRole('combobox'));
  await user.click(await screen.findByRole('option', { name: 'Development' }));
  await user.click(within(dialog).getByTestId('publish-terms-agree'));
}

describe('Publish of a governed agent', () => {
  it('opens Request publish for tier medium, creates and submits a change request, and shows its state', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await registryRead();
    await user.click(screen.getByTestId('agent-lifecycle-menu-button'));
    await user.click(screen.getByTestId('agent-publish-menuitem'));

    expect(screen.queryByTestId('publish-version-dialog')).not.toBeInTheDocument();
    await fillRequest(user);
    await user.click(screen.getByTestId('request-publish-submit'));

    expect(await screen.findByTestId('request-publish-outcome')).toHaveTextContent(
      'Change request #41 is Submitted. It needs 1 approval(s)',
    );
    expect(calls[0]?.body).toEqual({
      kind: 'release',
      version_id: 11,
      version_name: 'v2',
      category: 'Development',
      summary: 'Adds the refund policy.',
    });
    expect(calls[1]?.url).toContain('/change_requests/prompt_lib/2/7/41/submit');
  });

  it('opens the same dialog when a direct publish answers 409 change_request_required', async () => {
    registry = { ...REGISTRY, risk_tier: 'low' };
    server.use(
      http.post(`${CORE}/publish_validate/prompt_lib/:p/:v`, () =>
        HttpResponse.json({ status: 'PASS', critical_issues: [], warnings: [], recommendations: [], validation_token: 'tok' }),
      ),
      http.post(`${CORE}/publish/prompt_lib/:p/:v`, () =>
        refusal('change_request_required', 409, { risk_tier: 'medium', required_approvals: 1 }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(controls());
    await registryRead();
    await user.click(screen.getByTestId('agent-lifecycle-menu-button'));
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v2');
    await user.click(screen.getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Development' }));
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByTestId('publish-validation-status');
    await user.click(screen.getByRole('button', { name: 'Publish' }));

    expect(await screen.findByTestId('request-publish-dialog')).toBeInTheDocument();
    expect(screen.queryByText(/change_request_required/)).not.toBeInTheDocument();
  });
});

describe('refusal messages', () => {
  const error = (code: string, extra: Record<string, unknown> = {}) =>
    new EliteaApiError({ kind: 'http', status: 409, url: '/x', body: { error: code, code, message: 'm', ...extra } });

  it.each([
    ['self_approval_refused', 'You proposed this change, so someone else must approve it.'],
    ['approver_role_required', 'At this risk tier an approval must come from someone with the governance role.'],
    ['change_request_required', 'This agent’s risk tier needs an approved change request before it is published. Request publish instead.'],
    ['agent_certification_lapsed', 'This agent’s certification has lapsed, so its runs are refused until it is recertified.'],
  ])('%s reads as a sentence, not a code', (code, text) => {
    expect(lifecycleErrorMessage(error(code))).toBe(text);
  });

  it('names the failures of a failed gate and the missing attestations', () => {
    expect(lifecycleErrorMessage(error('release_gate_failed', { failures: ['gate_regression'] }))).toContain(
      '(gate_regression)',
    );
    expect(lifecycleErrorMessage(error('attestation_missing', { missing: ['legal', 'model_risk'] }))).toContain(
      '(legal, model_risk)',
    );
  });
});

describe('Governance dialog', () => {
  async function openChange(user: ReturnType<typeof userEvent.setup>): Promise<void> {
    renderWithProviders(
      <GovernanceDialog
        open
        projectId={PROJECT}
        applicationId={7}
        onClose={() => {}}
      />,
    );
    await user.click(screen.getByTestId('governance-tab-changes'));
    await user.click(await screen.findByTestId('change-request-open-41'));
    await screen.findByTestId('change-request-detail');
  }

  it('shows the gate failures and the regressed cases by name', async () => {
    const user = userEvent.setup();
    await openChange(user);
    expect(screen.getByTestId('change-request-gate-state')).toHaveTextContent('Failed');
    expect(await screen.findByTestId('change-request-gate-failures')).toHaveTextContent(
      '1 case regressed on the regression suite.',
    );
    expect(screen.getByTestId('change-request-gate-regressions')).toHaveTextContent('Refund after 30 days');
  });

  it('approves with the note, and says so when that approval published the version', async () => {
    let approved: unknown;
    server.use(
      http.post(`${CORE}/change_requests/prompt_lib/:p/:a/:cr/approve`, async ({ request }) => {
        approved = await request.json();
        return HttpResponse.json({
          change_request: { ...CHANGE, state: 'published' },
          complete: true,
          published: { version_id: 15, release_id: 6, version_name: 'v2' },
        });
      }),
    );
    const user = userEvent.setup();
    await openChange(user);
    await user.type(screen.getByTestId('change-request-note'), 'Looks right');
    await user.click(screen.getByTestId('change-request-approve'));
    expect(await screen.findByTestId('change-request-outcome')).toHaveTextContent('version v2 is published');
    expect(approved).toEqual({ note: 'Looks right' });
  });

  it('shows a refused approval as its sentence', async () => {
    server.use(
      http.post(`${CORE}/change_requests/prompt_lib/:p/:a/:cr/approve`, () => refusal('self_approval_refused', 403)),
    );
    const user = userEvent.setup();
    await openChange(user);
    await user.click(screen.getByTestId('change-request-approve'));
    expect(await screen.findByTestId('change-request-error')).toHaveTextContent(
      'You proposed this change, so someone else must approve it.',
    );
  });

  it('sends the gate override reason, capped at 200 characters', async () => {
    let body: unknown;
    server.use(
      http.post(`${CORE}/change_requests/prompt_lib/:p/:a/:cr/gate_override`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ...CHANGE, gate: { ...CHANGE.gate, overridden: true } });
      }),
    );
    const user = userEvent.setup();
    await openChange(user);
    await user.click(screen.getByTestId('change-request-override-reason'));
    await user.paste('x'.repeat(250));
    await user.click(screen.getByTestId('change-request-override'));
    await waitFor(() => expect(body).toEqual({ reason: 'x'.repeat(200) }));
  });

  it('warns when the certification lapsed', async () => {
    registry = { ...REGISTRY, certification_state: 'suspended' };
    renderWithProviders(
      <GovernanceDialog
        open
        projectId={PROJECT}
        applicationId={7}
        onClose={() => {}}
      />,
    );
    expect(await screen.findByTestId('governance-lapsed-banner')).toHaveTextContent('agent_certification_lapsed');
  });
});
