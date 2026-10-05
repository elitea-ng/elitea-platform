/**
 * Legacy issue 6669: the scope of a stored dimension is editable. An agent
 * dimension can move to the project library; a project dimension cannot move
 * back, so that option is disabled.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EVAL_ENGINE, EVAL_POLARITY, EVAL_SCALE_TYPE, EVAL_TIER } from '../model/types';
import { EvaluationLibraryView } from './EvaluationLibraryView';

const BASE = '/api/v2';

function dimensionRow(overrides: Record<string, unknown> = {}) {
  return {
    id: '1',
    uuid: 'e0f1',
    name: 'Faithfulness',
    description: 'Grounded?',
    tier: EVAL_TIER.project,
    application_id: null,
    allowed_engines: [EVAL_ENGINE.ai],
    scale_type: EVAL_SCALE_TYPE.continuous,
    scale_min: 0,
    scale_max: 100,
    polarity: EVAL_POLARITY.higherBetter,
    default_weight: 1,
    default_target: null,
    default_target_operator: '',
    code: '',
    return_contract: '',
    ...overrides,
  };
}

const ALL_DIMENSION_PERMISSIONS = [
  PERMISSIONS.evaluation.dimensionRead,
  PERMISSIONS.evaluation.dimensionCreate,
  PERMISSIONS.evaluation.dimensionUpdate,
  PERMISSIONS.evaluation.dimensionDelete,
];

function mockLibrary(
  row: ReturnType<typeof dimensionRow>,
  onPut: (body: Record<string, unknown>) => void,
  granted: readonly string[] = ALL_DIMENSION_PERMISSIONS,
): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(granted.map((name) => ({ name, enabled: true }))),
    ),
    http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: [row], total: 1 }),
    ),
    http.put(`${BASE}/elitea_core/eval_dimension/prompt_lib/:projectId/:dimensionId`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      onPut(body);
      return HttpResponse.json({ ...row, ...body, application_id: null });
    }),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('dimension scope on edit', () => {
  it('promotes an agent dimension to the project library', async () => {
    const puts: Record<string, unknown>[] = [];
    mockLibrary(dimensionRow({ tier: EVAL_TIER.agentAdhoc, application_id: 42 }), (body) => puts.push(body));
    const user = userEvent.setup();

    renderWithEvaluationProviders(<EvaluationLibraryView projectId="1" applicationId={42} />);
    await user.click(await screen.findByTestId('evaluation-dimension-edit-1'));

    const dialog = await screen.findByTestId('dimension-editor-dialog');
    expect(within(dialog).queryByTestId('dimension-tier-readonly')).not.toBeInTheDocument();
    await user.click(within(dialog).getByLabelText('Scope'));
    await user.click(await screen.findByRole('option', { name: 'Project library' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toMatchObject({ tier: EVAL_TIER.project, application_id: null });
  });

  // The server refuses a promotion without dimension.create (it adds a
  // library entry), so the option is disabled for an update-only author.
  it('does not offer the project library to an author who may update but not create', async () => {
    mockLibrary(dimensionRow({ tier: EVAL_TIER.agentAdhoc, application_id: 42 }), () => {}, [
      PERMISSIONS.evaluation.dimensionRead,
      PERMISSIONS.evaluation.dimensionUpdate,
    ]);
    const user = userEvent.setup();

    renderWithEvaluationProviders(<EvaluationLibraryView projectId="1" applicationId={42} />);
    await user.click(await screen.findByTestId('evaluation-dimension-edit-1'));

    const dialog = await screen.findByTestId('dimension-editor-dialog');
    await user.click(within(dialog).getByLabelText('Scope'));
    expect(await screen.findByRole('option', { name: 'Project library' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('option', { name: 'This agent only' })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('does not offer to move a project dimension back to one agent', async () => {
    mockLibrary(dimensionRow(), () => {});
    const user = userEvent.setup();

    renderWithEvaluationProviders(<EvaluationLibraryView projectId="1" applicationId={42} />);
    await user.click(await screen.findByTestId('evaluation-dimension-edit-1'));

    const dialog = await screen.findByTestId('dimension-editor-dialog');
    await user.click(within(dialog).getByLabelText('Scope'));
    expect(await screen.findByRole('option', { name: 'This agent only' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('option', { name: 'Project library' })).not.toHaveAttribute('aria-disabled', 'true');
  });
});
