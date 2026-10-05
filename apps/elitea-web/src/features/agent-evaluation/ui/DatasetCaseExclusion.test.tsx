/**
 * Legacy issue 6700: include / exclude a dataset case with a checkbox, an
 * "N active" chip, the "shared" icon to the left of the dataset name, the
 * two-tone disabled state of an excluded case, and the read-only "View
 * details" modal.
 */
import { screen, waitFor, within } from '@testing-library/react';
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
    active_case_count: 1,
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
  it('counts the active cases and toggles one with a PUT carrying `excluded` alone', async () => {
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
    // The flag ALONE. The cached case text in this body would overwrite any
    // edit made in another tab since this view last read the dataset.
    expect(bodies).toEqual([{ excluded: false }]);

    await userEvent.click(screen.getAllByRole('checkbox', { name: 'Include this case in runs' })[0]!);
    await waitFor(() => expect(screen.getByTestId('dataset-cases-active-count')).toHaveTextContent('1 active'));
    expect(bodies[1]).toEqual({ excluded: true });
  });

  it('says why a refused toggle or remove did nothing', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead, PERMISSIONS.evaluation.datasetUpdate]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [caseRow('11', false)], cases_truncated: false }),
      ),
      http.put(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, () =>
        HttpResponse.json({ error: 'case not found in this dataset' }, { status: 404 }),
      ),
      http.delete(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, () =>
        HttpResponse.json({ error: 'missing permission' }, { status: 403 }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    await userEvent.click(await screen.findByRole('checkbox', { name: 'Include this case in runs' }));
    expect(await screen.findByTestId('dataset-case-toggle-error')).toHaveTextContent('case not found in this dataset');
    // The stored state is unchanged, and the box says so once the refetch lands.
    await waitFor(() =>
      expect(screen.getByRole('checkbox', { name: 'Include this case in runs' })).toBeChecked(),
    );

    await userEvent.click(screen.getByTestId('dataset-case-remove-11'));
    expect(await screen.findByTestId('dataset-case-remove-error')).toHaveTextContent('missing permission');
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

  it('paints an excluded case in two disabled tones, the expected line dimmer than the input', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [caseRow('12', true)], cases_truncated: false }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    const input = await screen.findByTestId('dataset-case-input-text-12');
    const expected = screen.getByTestId('dataset-case-expected-text-12');
    // gray10 / light10 for the primary line, gray20 / light20 for the secondary.
    expect(getComputedStyle(input).color).toBe('var(--el-palette-text-primary)');
    expect(getComputedStyle(expected).color).toBe('var(--el-palette-text-button-disabled)');
  });

  it('disables only the row whose toggle is in flight', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead, PERMISSIONS.evaluation.datasetUpdate]);
    const cases = [caseRow('11', false), caseRow('12', false)];
    const releases: Array<() => void> = [];
    const bodies: Array<{ caseId: string; excluded: boolean }> = [];
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
          bodies.push({ caseId: String(params.caseId), excluded: body.excluded });
          await new Promise<void>((resolve) => releases.push(resolve));
          const index = cases.findIndex((entry) => entry.id === params.caseId);
          cases[index] = { ...cases[index]!, excluded: body.excluded };
          return HttpResponse.json(cases[index]);
        },
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    const first = within(await screen.findByTestId('dataset-case-11')).getByRole('checkbox');
    const second = within(screen.getByTestId('dataset-case-12')).getByRole('checkbox');
    await userEvent.click(first);
    await waitFor(() => expect(first).toBeDisabled());
    // The other case is still clickable while the first PUT is pending.
    expect(second).toBeEnabled();
    await userEvent.click(second);
    await waitFor(() => expect(releases).toHaveLength(2));
    expect(bodies).toEqual([
      { caseId: '11', excluded: true },
      { caseId: '12', excluded: true },
    ]);

    releases.forEach((release) => release());
    await waitFor(() => expect(screen.getByTestId('dataset-cases-active-count')).toHaveTextContent('0 active'));
    expect(first).toBeEnabled();
    expect(second).toBeEnabled();
  });

  it('keeps the first toggle’s refusal when a second toggle succeeds', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead, PERMISSIONS.evaluation.datasetUpdate]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [caseRow('11', false), caseRow('12', false)], cases_truncated: false }),
      ),
      http.put(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, ({ params }) =>
        params.caseId === '11'
          ? HttpResponse.json({ error: 'case not found in this dataset' }, { status: 404 })
          : HttpResponse.json({ ...caseRow('12', true) }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    await userEvent.click(within(await screen.findByTestId('dataset-case-11')).getByRole('checkbox'));
    expect(await screen.findByTestId('dataset-case-toggle-error')).toHaveTextContent('case not found in this dataset');
    await userEvent.click(within(screen.getByTestId('dataset-case-12')).getByRole('checkbox'));
    await waitFor(() =>
      expect(within(screen.getByTestId('dataset-case-12')).getByRole('checkbox')).toBeEnabled(),
    );
    expect(screen.getByTestId('dataset-case-toggle-error')).toHaveTextContent('case not found in this dataset');
  });

  it('opens a read-only case details modal from the row’s View details button', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead, PERMISSIONS.evaluation.datasetUpdate]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [datasetRow()], total: 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({
          ...datasetRow(),
          cases: [{ ...caseRow('11', false), expected_output: 'Goroutines' }],
          cases_truncated: false,
        }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    await userEvent.click(await screen.findByTestId('dataset-case-view-details-11'));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Case details')).toBeInTheDocument();
    expect(within(dialog).getByTestId('dataset-case-details-input')).toHaveValue('question 11');
    expect(within(dialog).getByTestId('dataset-case-details-input')).toBeDisabled();
    expect(within(dialog).getByTestId('dataset-case-details-expected')).toHaveValue('Goroutines');
    expect(within(dialog).getByTestId('dataset-case-details-expected')).toBeDisabled();
    expect(within(dialog).getByTestId('dataset-case-details-variables')).toHaveValue(
      JSON.stringify({ topic: 'Go' }, null, 2),
    );
    expect(within(dialog).getByTestId('dataset-case-details-variables')).toBeDisabled();
    // No Save and no Cancel: the close control is the only button.
    expect(within(dialog).queryByRole('button', { name: /save|cancel/i })).not.toBeInTheDocument();

    await userEvent.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});
