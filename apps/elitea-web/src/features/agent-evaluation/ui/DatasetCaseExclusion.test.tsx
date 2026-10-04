/**
 * Legacy issue 6700: include / exclude a dataset case with a checkbox, an
 * "N active" chip, and the "shared" icon to the left of the dataset name.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EvaluationDatasetsView } from './EvaluationDatasetsView';

const BASE = '/api/v2';

function mockPermissions(names: readonly string[]): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(names.map((name) => ({ name, enabled: true }))),
    ),
  );
}

function datasetRow(overrides: Record<string, unknown> = {}) {
  return {
    id: '5',
    uuid: 'd-5',
    name: 'Support answers',
    description: '',
    application_id: null,
    is_shared: false,
    case_count: 2,
    ...overrides,
  };
}

function caseRow(id: string, excluded: boolean) {
  return {
    id,
    dataset_id: '5',
    input: `question ${id}`,
    variables: { topic: 'Go' },
    expected_output: null,
    source_type: 'manual',
    order_index: Number(id),
    excluded,
  };
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('dataset case exclusion', () => {
  it('counts the active cases and toggles one with a full PUT carrying `excluded`', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead, PERMISSIONS.evaluation.datasetUpdate]);
    const cases = [caseRow('11', false), caseRow('12', true)];
    const bodies: unknown[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases, cases_truncated: false }),
      ),
      http.put(
        `${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`,
        async ({ request, params }) => {
          const body = (await request.json()) as { excluded: boolean };
          bodies.push(body);
          const index = cases.findIndex((entry) => entry.id === params.caseId);
          cases[index] = { ...cases[index]!, excluded: body.excluded };
          return HttpResponse.json(cases[index]);
        },
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    expect(await screen.findByTestId('dataset-cases-active-count')).toHaveTextContent('1 active');
    const included = screen.getByRole('checkbox', { name: 'Include this case in runs', checked: true });
    const excluded = screen.getByRole('checkbox', { name: 'Include this case in runs', checked: false });
    expect(included).toBeEnabled();
    expect(excluded).toBeEnabled();
    expect(screen.getByTestId('dataset-case-12').querySelector('[data-excluded="true"]')).not.toBeNull();
    expect(screen.getByTestId('dataset-case-11').querySelector('[data-excluded="true"]')).toBeNull();

    // No confirmation dialog: the toggle is reversible and deletes nothing.
    await userEvent.click(excluded);
    await waitFor(() => expect(screen.getByTestId('dataset-cases-active-count')).toHaveTextContent('2 active'));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(bodies).toEqual([
      { input: 'question 12', variables: { topic: 'Go' }, expected_output: null, excluded: false },
    ]);

    await userEvent.click(screen.getAllByRole('checkbox', { name: 'Include this case in runs' })[0]!);
    await waitFor(() => expect(screen.getByTestId('dataset-cases-active-count')).toHaveTextContent('1 active'));
    expect(bodies[1]).toMatchObject({ input: 'question 11', excluded: true });
  });

  it('shows the checkbox read-only to a caller who may not update the dataset', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [caseRow('11', true)], cases_truncated: false }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    expect(await screen.findByRole('checkbox', { name: 'Include this case in runs' })).toBeDisabled();
    expect(screen.getByTestId('dataset-cases-active-count')).toHaveTextContent('0 active');
  });

  it('puts the shared icon to the left of a shared dataset’s name, and only there', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [datasetRow({ is_shared: true }), datasetRow({ id: '6', name: 'Private set', is_shared: false })],
          total: 2,
        }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    const name = await screen.findByText('Support answers');
    const icon = screen.getByTestId('evaluation-dataset-shared-5');
    // DOCUMENT_POSITION_FOLLOWING: the name comes after the icon.
    expect(icon.compareDocumentPosition(name) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(icon).toHaveAccessibleName('Shared dataset');
    expect(screen.queryByTestId('evaluation-dataset-shared-6')).not.toBeInTheDocument();
  });
});
