import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EvaluationRunsView } from './EvaluationRunsView';

const BASE = '/api/v2';

const ALL_RUN_PERMISSIONS = [
  PERMISSIONS.evaluation.runRead,
  PERMISSIONS.evaluation.runCreate,
  PERMISSIONS.evaluation.datasetRead,
  PERMISSIONS.evaluation.dimensionRead,
];

function mockPermissions(names: readonly string[]): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(names.map((name) => ({ name, enabled: true }))),
    ),
  );
}

function runRow(overrides: Record<string, unknown> = {}) {
  return {
    id: '9',
    dataset_id: '5',
    status: 'finished',
    execution_mode: 'predict_blocking',
    progress: { done: 2, total: 2 },
    headline_score: 75,
    ...overrides,
  };
}

function dimensionRow(overrides: Record<string, unknown> = {}) {
  return {
    id: '3',
    name: 'Helpfulness',
    description: 'Does it help?',
    tier: 'project',
    application_id: null,
    allowed_engines: ['ai'],
    scale_type: 'ordinal',
    scale_min: 1,
    scale_max: 5,
    polarity: 'higher_better',
    default_weight: 1,
    default_target: null,
    default_target_operator: '',
    code: '',
    return_contract: '',
    ...overrides,
  };
}

function mockSupportingReads(options: {
  readonly runs?: readonly unknown[];
  readonly datasets?: readonly unknown[];
  readonly dimensions?: readonly unknown[];
} = {}): void {
  server.use(
    http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: options.runs ?? [], total: (options.runs ?? []).length }),
    ),
    http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: options.datasets ?? [], total: (options.datasets ?? []).length }),
    ),
    http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: options.dimensions ?? [], total: (options.dimensions ?? []).length }),
    ),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('EvaluationRunsView', () => {
  /*
   * A NULL HEADLINE IS SAID, NOT PRINTED AS 0. Zero is a real and very bad
   * score; a run that scored nothing has not earned it, and printing it would
   * report a broken judge as a broken agent.
   */
  it('says a run has no score rather than showing zero', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({ runs: [runRow({ status: 'errored', headline_score: null, error: 'no LLM plane' })] });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByTestId('evaluation-run-row-9')).toHaveTextContent('No score');
    expect(screen.getByTestId('evaluation-run-row-9')).not.toHaveTextContent('Score 0');
    // The run's own failure reason is shown, not swallowed. Scoped to the ROW:
    // the start form also renders an alert (it names why a run cannot be
    // started), and a bare `getByRole('alert')` would be ambiguous.
    expect(screen.getByTestId('evaluation-run-row-9')).toHaveTextContent('no LLM plane');
  });

  it('shows progress for every run, terminal or not', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({ runs: [runRow({ status: 'finished', progress: { done: 2, total: 10 } })] });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    // A run that FINISHED having scored two of ten cases is a different fact
    // from one that scored all ten, and the headline alone cannot say which.
    expect(await screen.findByTestId('evaluation-run-row-9')).toHaveTextContent('2 of 10 case(s)');
  });

  it('does not ask the server when the caller may not read runs', async () => {
    mockPermissions([]);
    let asked = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () => {
        asked = true;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByRole('status')).toHaveTextContent('do not have permission');
    expect(asked).toBe(false);
  });

  /*
   * A `code` dimension is NOT offered. Offering it would produce a 501 the
   * person could not have predicted from the form.
   */
  it('offers only ai dimensions to score against', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({
      datasets: [{ id: '5', name: 'Support', description: '', application_id: null, is_shared: false, case_count: 2 }],
      dimensions: [
        dimensionRow(),
        dimensionRow({ id: '4', name: 'JSON shape', allowed_engines: ['code'] }),
      ],
    });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByTestId('run-dimension-3')).toBeInTheDocument();
    expect(screen.queryByTestId('run-dimension-4')).not.toBeInTheDocument();
  });

  /*
   * THE START BUTTON'S REFUSAL IS STATED. "Disabled" is true for four different
   * causes, and a control that does nothing when pressed is how each of them
   * reads as a broken feature.
   */
  it('names the reason a run cannot be started yet', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({
      datasets: [{ id: '5', name: 'Empty set', description: '', application_id: null, is_shared: false, case_count: 0 }],
      dimensions: [dimensionRow()],
    });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={undefined} />,
    );

    expect(await screen.findByTestId('run-start-error')).toHaveTextContent('Save this agent');
    expect(screen.getByTestId('run-start')).toBeDisabled();
  });

  it('starts a run and sends the version the editor has open', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    let sent: unknown;
    mockSupportingReads({
      datasets: [{ id: '5', name: 'Support', description: '', application_id: null, is_shared: false, case_count: 2 }],
      dimensions: [dimensionRow()],
    });
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(runRow({ status: 'created' }), { status: 201 });
      }),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    await userEvent.click(await screen.findByTestId('run-dimension-3'));
    // The MUI select is a listbox, not a native <select>.
    await userEvent.click(screen.getByRole('combobox'));
    await userEvent.click(await screen.findByRole('option', { name: 'Support' }));
    await userEvent.click(screen.getByTestId('run-start'));

    await waitFor(() =>
      expect(sent).toMatchObject({
        dataset_id: '5',
        application_version_id: 9,
        dimension_ids: ['3'],
        trigger_type: 'on_demand',
      }),
    );
  });

  /*
   * A 501 REACHES THE PERSON WITH ITS REASON. The transport message says only
   * "501 from …", which nobody can act on; the server's body names the engine
   * and why this release does not serve it.
   */
  it('shows the server’s 501 refusal text when a dimension cannot be scored', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({
      datasets: [{ id: '5', name: 'Support', description: '', application_id: null, is_shared: false, case_count: 2 }],
      dimensions: [dimensionRow()],
    });
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
        HttpResponse.json(
          { error: 'dimension JSON shape cannot be scored by this release: the code engine needs a sandbox' },
          { status: 501 },
        ),
      ),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    await userEvent.click(await screen.findByTestId('run-dimension-3'));
    await userEvent.click(screen.getByRole('combobox'));
    await userEvent.click(await screen.findByRole('option', { name: 'Support' }));
    await userEvent.click(screen.getByTestId('run-start'));

    expect(await screen.findByTestId('run-start-error')).toHaveTextContent('needs a sandbox');
  });

  it('offers a stop control only while a run is still active', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({
      runs: [runRow({ id: '9', status: 'running' }), runRow({ id: '10', status: 'finished' })],
    });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByTestId('evaluation-run-cancel-9')).toBeInTheDocument();
    expect(screen.queryByTestId('evaluation-run-cancel-10')).not.toBeInTheDocument();
  });

  /*
   * THE SCORECARD NAMES WHAT IT CANNOT SERVE. Rendering `unavailable` is what
   * stops "no human scores shown" from reading as "nobody has entered one".
   */
  it('opens a read-only scorecard that names its own gaps and how the run was executed', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({ runs: [runRow()] });
    server.use(
      http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({
          run: runRow(),
          results: [
            {
              id: '1',
              run_id: '9',
              dataset_case_id: '11',
              dimension_id: '3',
              status: 'ok',
              native_score: 5,
              normalized_score: 100,
              target_met: true,
              verdict: { score: 5, reason: 'answered the question' },
              evidence: { input: 'q', output: 'a' },
            },
            {
              id: '2',
              run_id: '9',
              dataset_case_id: '12',
              dimension_id: '3',
              status: 'error',
              native_score: null,
              normalized_score: null,
              target_met: null,
              verdict: { error: 'the judge did not answer in the score schema', raw: 'about four' },
              evidence: { input: 'q2', output: 'a2' },
            },
          ],
          headline_score: 100,
          total: 2,
          offset: 0,
          unavailable: [
            { key: 'human_scores', reason: 'this release has no human-score surface' },
          ],
        }),
      ),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    expect(await screen.findByTestId('scorecard-headline')).toHaveTextContent('Overall 100 / 100');
    // WHAT ACTUALLY RAN, so a reader does not take these numbers for a
    // measurement of the whole agent.
    expect(screen.getByTestId('scorecard-execution-mode')).toHaveTextContent('Tools and toolkits were not used');
    // The scored case shows its number; the unscorable one shows its STATUS
    // and reason, and never a zero.
    expect(screen.getByTestId('scorecard-result-1')).toHaveTextContent('100 / 100');
    const errored = screen.getByTestId('scorecard-result-2');
    expect(errored).toHaveTextContent('Not scored (error)');
    expect(errored).toHaveTextContent('did not answer in the score schema');
    expect(errored).not.toHaveTextContent('0 / 100');
    expect(screen.getByTestId('scorecard-unavailable')).toHaveTextContent('no human-score surface');
  });
  it('cancels a running run and shows it stopped', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    let cancelled = false;
    server.use(
      http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [runRow({ status: cancelled ? 'cancelled' : 'running' })],
          total: 1,
        }),
      ),
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [], total: 0 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [], total: 0 }),
      ),
      http.post(`${BASE}/elitea_core/eval_run_cancel/prompt_lib/:projectId/:runId`, () => {
        cancelled = true;
        return HttpResponse.json(runRow({ status: 'cancelled' }));
      }),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    await userEvent.click(await screen.findByTestId('evaluation-run-cancel-9'));

    // The listing is REFRESHED after the write. A cancel that answered 200 and
    // left "running" on the screen is a write that looks like it did nothing.
    await waitFor(() => expect(screen.getByTestId('evaluation-run-row-9')).toHaveTextContent('cancelled'));
  });

  it('says a project has no runs rather than rendering nothing', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({ runs: [] });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByText('No runs yet.')).toBeInTheDocument();
  });

  it('reports a failed run listing as an error rather than as an empty one', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    server.use(
      http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [], total: 0 }),
      ),
      http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [], total: 0 }),
      ),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    expect(await screen.findByText('Failed to load the evaluation runs.')).toBeInTheDocument();
  });

  /*
   * A run still in flight says SO on its scorecard, from the run poll and not
   * from the snapshot embedded in the results page. Reading the snapshot would
   * leave a scorecard opened on a running run frozen at the progress it had
   * when the page was fetched, which reads as a run that stopped.
   */
  it('shows live progress on the scorecard of a run that is still going', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    const running = runRow({ status: 'running', progress: { done: 1, total: 4 }, headline_score: null });
    mockSupportingReads({ runs: [running] });
    server.use(
      http.get(`${BASE}/elitea_core/eval_run/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json(runRow({ status: 'running', progress: { done: 3, total: 4 }, headline_score: null })),
      ),
      http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({
          run: running,
          results: [],
          headline_score: null,
          total: 0,
          offset: 0,
          unavailable: [],
        }),
      ),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );
    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    // 3 of 4 comes from the run POLL. The results page's own snapshot says 1.
    expect(await screen.findByTestId('scorecard-progress')).toHaveTextContent('3 of 4 case(s)');
    expect(screen.getByTestId('scorecard-progress')).toHaveTextContent('still running');
    expect(screen.getByTestId('scorecard-headline')).toHaveTextContent('produced no score');
  });

  it('reports a failed scorecard read', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({ runs: [runRow()] });
    server.use(
      http.get(`${BASE}/elitea_core/eval_run/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json(runRow()),
      ),
      http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({ error: 'gone' }, { status: 500 }),
      ),
    );

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );
    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    expect(await screen.findByText(/Failed to load this run/)).toBeInTheDocument();
  });
  /*
   * EACH BLOCKING REASON IS ITS OWN MESSAGE. "Disabled" is one state with four
   * causes, and a person who is told the wrong one looks in the wrong place.
   */
  it('names each reason a run is blocked, one at a time', async () => {
    mockPermissions(ALL_RUN_PERMISSIONS);
    mockSupportingReads({
      datasets: [
        { id: '5', name: 'Empty set', description: '', application_id: null, is_shared: false, case_count: 0 },
        { id: '6', name: 'Full set', description: '', application_id: null, is_shared: false, case_count: 2 },
      ],
      dimensions: [
        dimensionRow(),
        dimensionRow({ id: '31', name: 'B' }),
        dimensionRow({ id: '32', name: 'C' }),
        dimensionRow({ id: '33', name: 'D' }),
        dimensionRow({ id: '34', name: 'E' }),
        dimensionRow({ id: '35', name: 'F' }),
      ],
    });

    renderWithEvaluationProviders(
      <EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />,
    );

    // 1. No dataset chosen yet.
    expect(await screen.findByTestId('run-start-error')).toHaveTextContent('Choose a dataset.');

    // 2. A dataset with no cases: a run over nothing finishes instantly with no
    //    headline, which reads as a broken agent.
    await userEvent.click(screen.getByRole('combobox'));
    await userEvent.click(await screen.findByRole('option', { name: 'Empty set' }));
    expect(screen.getByTestId('run-start-error')).toHaveTextContent('has no cases');

    // 3. A usable dataset, but nothing to score against.
    await userEvent.click(screen.getByRole('combobox'));
    await userEvent.click(await screen.findByRole('option', { name: 'Full set' }));
    expect(screen.getByTestId('run-start-error')).toHaveTextContent('at least one dimension');

    // 4. Past the server's own fan-out bound. A run is cases x (1 + dimensions)
    //    model calls, so the cap is a cost decision and not a UI whim.
    for (const id of ['3', '31', '32', '33', '34', '35']) {
      await userEvent.click(screen.getByTestId(`run-dimension-${id}`));
    }
    expect(screen.getByTestId('run-start-error')).toHaveTextContent('at most 5 dimensions');
    expect(screen.getByTestId('run-start')).toBeDisabled();
  }, 20_000);
});
