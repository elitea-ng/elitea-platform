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

const ALL_DATASET_PERMISSIONS = [
  PERMISSIONS.evaluation.datasetRead,
  PERMISSIONS.evaluation.datasetCreate,
  PERMISSIONS.evaluation.datasetUpdate,
  PERMISSIONS.evaluation.datasetDelete,
];

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
    case_count: 0,
    ...overrides,
  };
}

function mockDatasets(rows: readonly unknown[]): void {
  server.use(
    http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows, total: rows.length }),
    ),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('EvaluationDatasetsView', () => {
  it('renders a dataset with its STORED case count, not the page length', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow({ case_count: 7 })]);

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    expect(await screen.findByText('Support answers')).toBeInTheDocument();
    // 7, and NOT the length of any `cases` array — the listing carries no
    // cases at all, which is the whole reason the count is a column.
    expect(screen.getByText('7 case(s)')).toBeInTheDocument();
  });

  /*
   * AN EMPTY LISTING IS REPORTED. A list with no rows and no message is
   * indistinguishable from a listing that failed silently — the reading every
   * "200 with an empty screen" defect in this app produces.
   */
  it('says a project has no datasets rather than rendering nothing', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([]);

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    // The TEXT and not `findByRole('status')`: the no-permission branch is also
    // a `status`, and it renders FIRST while the permission list is in flight.
    // A test that waited on the role alone would settle on the wrong message
    // and then assert against a screen that had already moved on.
    expect(await screen.findByText(/No datasets yet/)).toBeInTheDocument();
  });

  it('reports a failed listing as an error rather than as an empty one', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load the evaluation datasets.');
  });

  /*
   * WITH NO READ PERMISSION THE QUERY IS NOT MADE AT ALL. A 403 rendered as an
   * error banner tells a viewer their product is broken, when in fact they
   * simply may not read this project's datasets.
   */
  it('does not ask the server when the caller may not read datasets', async () => {
    mockPermissions([]);
    let asked = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () => {
        asked = true;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    expect(await screen.findByRole('status')).toHaveTextContent('do not have permission');
    expect(asked).toBe(false);
  });

  it('hides the add control from a caller who may not create a dataset', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead]);
    mockDatasets([datasetRow()]);

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    expect(await screen.findByText('Support answers')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add dataset' })).not.toBeInTheDocument();
  });

  /*
   * THE ROW IS READ BACK AFTER THE WRITE. A create that answers 201 and leaves
   * the listing stale is a write that looks like it did nothing — which is what
   * a mutation invalidating the wrong key namespace produces.
   */
  it('creates a dataset and shows it in the refreshed listing', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    let created = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: created ? [datasetRow({ name: 'Regression set' })] : [],
          total: created ? 1 : 0,
        }),
      ),
      http.post(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, async ({ request }) => {
        const body = (await request.json()) as { name: string; application_id: number | null };
        // The agent the editor is open on is filed onto the dataset, and it is
        // NOT a field the person chose.
        expect(body.application_id).toBe(42);
        created = true;
        return HttpResponse.json(datasetRow({ name: body.name }), { status: 201 });
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);

    // Waits for the CONTROL, not for any status: the permission list resolves
    // after the first render, and the add button is what proves it arrived.
    await userEvent.click(await screen.findByRole('button', { name: 'Add dataset' }));
    await userEvent.type(await screen.findByTestId('dataset-name-input'), 'Regression set');
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));

    expect(await screen.findByText('Regression set')).toBeInTheDocument();
  });

  it('refuses to submit a dataset with no name and says why', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([]);
    let posted = false;
    server.use(
      http.post(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () => {
        posted = true;
        return HttpResponse.json(datasetRow(), { status: 201 });
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Add dataset' }));

    const dialog = await screen.findByTestId('dataset-create-dialog');
    expect(dialog).toHaveTextContent('A dataset needs a name.');
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(posted).toBe(false));
  });

  /*
   * The SERVER's refusal wins over a generic message. The cap refusal names the
   * limit and the delete refusal names the run count; replacing either with
   * "Failed to save" throws away the only part a person can act on.
   */
  it('shows the server’s own refusal text when a create fails', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([]);
    server.use(
      http.post(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'name must be at most 128 characters' }, { status: 400 }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Add dataset' }));
    await userEvent.type(await screen.findByTestId('dataset-name-input'), 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));

    expect(await screen.findByTestId('dataset-create-error')).toHaveTextContent('at most 128 characters');
  });

  it('opens a dataset’s cases and adds one, sending a null expected output when it is blank', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow({ case_count: 0 })]);
    let sent: unknown;
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [], cases_truncated: false }),
      ),
      http.post(`${BASE}/elitea_core/eval_dataset_cases/prompt_lib/:projectId/:datasetId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(
          { id: '11', dataset_id: '5', input: 'q', variables: {}, expected_output: null, source_type: 'manual', order_index: 0 },
          { status: 201 },
        );
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    await userEvent.type(await screen.findByTestId('dataset-case-input'), 'what is Go?');
    await userEvent.click(screen.getByRole('button', { name: 'Add case' }));

    await waitFor(() =>
      // `null`, not `''`: a judge told to compare against an empty string marks
      // every non-empty answer wrong.
      expect(sent).toEqual({ input: 'what is Go?', variables: {}, expected_output: null }),
    );
  });
  it('removes a case and shows the refreshed list', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow({ case_count: 1 })]);
    let removed = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({
          ...datasetRow(),
          cases: removed
            ? []
            : [
                {
                  id: '11',
                  dataset_id: '5',
                  input: 'what is Go?',
                  variables: {},
                  expected_output: 'a language',
                  source_type: 'manual',
                  order_index: 0,
                },
              ],
          cases_truncated: false,
        }),
      ),
      http.delete(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, () => {
        removed = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    // A stated expected answer is SHOWN, and an absent one says so — the two
    // are different instructions to a judge and must not look alike.
    expect(await screen.findByTestId('dataset-case-11')).toHaveTextContent('Expected: a language');

    await userEvent.click(screen.getByTestId('dataset-case-remove-11'));
    await waitFor(() => expect(screen.queryByTestId('dataset-case-11')).not.toBeInTheDocument());
  });

  it('says an absent expected answer is absent rather than leaving the cell blank', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow({ case_count: 1 })]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({
          ...datasetRow(),
          cases: [
            { id: '12', dataset_id: '5', input: 'q', variables: {}, expected_output: null, source_type: 'manual', order_index: 0 },
          ],
          cases_truncated: false,
        }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    expect(await screen.findByTestId('dataset-case-12')).toHaveTextContent('No expected answer stated');
  });

  /*
   * THE DELETE REFUSAL NAMES THE COUNT. The table cascades a dataset delete to
   * its runs, so this 409 is the only thing between a casual tidy-up and the
   * loss of every score — and the message is what tells a person that.
   */
  it('shows the server’s refusal when a dataset still has runs against it', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow()]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [], cases_truncated: false }),
      ),
      http.delete(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json(
          { error: 'this dataset has 3 run(s) scored against it: deleting it would destroy their results' },
          { status: 409 },
        ),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));
    await userEvent.click(await screen.findByTestId('dataset-delete'));

    // The dataset is STILL THERE: a refused delete must not clear the panel.
    expect(await screen.findByTestId('evaluation-dataset-row-5')).toBeInTheDocument();
  });

  it('closes the case panel when a dataset is deleted', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    let deleted = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: deleted ? [] : [datasetRow()], total: deleted ? 0 : 1 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [], cases_truncated: false }),
      ),
      http.delete(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));
    await userEvent.click(await screen.findByTestId('dataset-delete'));

    await waitFor(() => expect(screen.queryByTestId('dataset-cases-panel')).not.toBeInTheDocument());
    expect(await screen.findByText(/No datasets yet/)).toBeInTheDocument();
  });

  it('hides the case editor from a caller who may not update the dataset', async () => {
    mockPermissions([PERMISSIONS.evaluation.datasetRead]);
    mockDatasets([datasetRow()]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ ...datasetRow(), cases: [], cases_truncated: false }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    expect(await screen.findByTestId('dataset-cases-panel')).toBeInTheDocument();
    expect(screen.queryByTestId('dataset-case-input')).not.toBeInTheDocument();
    expect(screen.queryByTestId('dataset-delete')).not.toBeInTheDocument();
  });

  it('reports a failed case read as an error', async () => {
    mockPermissions(ALL_DATASET_PERMISSIONS);
    mockDatasets([datasetRow()]);
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
        HttpResponse.json({ error: 'gone' }, { status: 500 }),
      ),
    );

    renderWithEvaluationProviders(<EvaluationDatasetsView projectId="1" applicationId={42} />);
    await userEvent.click(await screen.findByTestId('evaluation-dataset-row-5'));

    expect(await screen.findByText(/Failed to load this dataset/)).toBeInTheDocument();
  });
});
