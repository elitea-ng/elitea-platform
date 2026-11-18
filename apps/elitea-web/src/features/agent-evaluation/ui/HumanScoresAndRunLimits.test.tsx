import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EvaluationRunsView } from './EvaluationRunsView';

const BASE = '/api/v2';

const READ = [
  PERMISSIONS.evaluation.runRead,
  PERMISSIONS.evaluation.datasetRead,
  PERMISSIONS.evaluation.dimensionRead,
  PERMISSIONS.evaluation.humanScoreRead,
];
const WRITE = [...READ, PERMISSIONS.evaluation.runCreate, PERMISSIONS.evaluation.humanScoreCreate];

function mockPermissions(names: readonly string[]): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(names.map((name) => ({ name, enabled: true }))),
    ),
  );
}

const humanDimension = { name: 'Tone', scale_type: 'ordinal', scale_min: 1, scale_max: 5, polarity: 'higher_better' };

function run(overrides: Record<string, unknown> = {}) {
  return {
    id: '9',
    dataset_id: '5',
    status: 'finished',
    execution_mode: 'predict_blocking',
    progress: { done: 1, total: 1 },
    headline_score: null,
    snapshot: {
      cases: [{ id: '11', order_index: 0 }],
      dimensions: { '7': humanDimension },
      bindings: [{ dimension_id: '7', engine: 'human', weight: 1, target_operator: '', order_index: 0 }],
    },
    ...overrides,
  };
}

function pendingResult() {
  return {
    id: '1',
    run_id: '9',
    dataset_case_id: '11',
    dimension_id: '7',
    status: 'pending_human',
    native_score: null,
    normalized_score: null,
    target_met: null,
    verdict: { pending: 'waiting for a human score' },
    evidence: { input: 'q', output: 'a' },
  };
}

function mockReads(options: { runs?: unknown[]; suites?: unknown[]; scores?: unknown[]; results?: unknown[] } = {}): void {
  server.use(
    http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: options.runs ?? [run()], total: 1 }),
    ),
    http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/elitea_core/eval_suites/prompt_lib/:projectId`, () =>
      HttpResponse.json({ rows: options.suites ?? [], total: (options.suites ?? []).length }),
    ),
    http.get(`${BASE}/elitea_core/eval_human_scores/prompt_lib/:projectId/:runId`, () =>
      HttpResponse.json({ rows: options.scores ?? [], total: (options.scores ?? []).length }),
    ),
    http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
      HttpResponse.json({
        run: options.runs?.[0] ?? run(),
        results: options.results ?? [pendingResult()],
        headline_score: null,
        total: 1,
        offset: 0,
        unavailable: [],
      }),
    ),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

function renderView(): void {
  renderWithEvaluationProviders(<EvaluationRunsView projectId="1" applicationId={42} applicationVersionId={9} />);
}

describe('human scores on the scorecard', () => {
  it('shows a pending result as waiting and records a score against the frozen scale', async () => {
    mockPermissions(WRITE);
    mockReads();
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_human_scores/prompt_lib/:projectId/:runId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(
          {
            human_score: { id: '1', run_id: '9', dataset_case_id: '11', dimension_id: '7', native_score: 4, normalized_score: 75, note: 'warm', reviewer_id: 3 },
            result: { ...pendingResult(), status: 'ok', native_score: 4, normalized_score: 75 },
            headline_score: 75,
          },
          { status: 201 },
        );
      }),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));
    const row = await screen.findByTestId('scorecard-result-1');
    expect(row).toHaveTextContent('Waiting for a human score');
    // Not a zero: a pending result has no score yet.
    expect(row).not.toHaveTextContent('0 / 100');

    await userEvent.type(screen.getByTestId('human-score-input-11-7'), '4');
    await userEvent.type(screen.getByTestId('human-score-note-11-7'), 'warm');
    await userEvent.click(screen.getByTestId('human-score-save-11-7'));

    await waitFor(() => expect(sent).toEqual({ dataset_case_id: '11', dimension_id: '7', score: 4, note: 'warm' }));
  });

  it('refuses a score outside the dimension scale before sending it', async () => {
    mockPermissions(WRITE);
    mockReads();
    let posted = false;
    server.use(
      http.post(`${BASE}/elitea_core/eval_human_scores/prompt_lib/:projectId/:runId`, () => {
        posted = true;
        return HttpResponse.json({}, { status: 201 });
      }),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));
    await userEvent.type(await screen.findByTestId('human-score-input-11-7'), '9');

    expect(screen.getByTestId('human-score-11-7')).toHaveTextContent('Score between 1 and 5.');
    expect(screen.getByTestId('human-score-save-11-7')).toBeDisabled();
    expect(posted).toBe(false);
  });

  it('shows who scored and the note, and offers a re-score', async () => {
    mockPermissions(WRITE);
    mockReads({
      results: [{ ...pendingResult(), status: 'ok', native_score: 4, normalized_score: 75, verdict: { score: 4, scored_by: 'human' } }],
      scores: [
        { id: '1', run_id: '9', dataset_case_id: '11', dimension_id: '7', native_score: 3, normalized_score: 50, note: 'first', reviewer_id: 3 },
        { id: '2', run_id: '9', dataset_case_id: '11', dimension_id: '7', native_score: 4, normalized_score: 75, note: 'warm', reviewer_id: 4 },
      ],
    });
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    // The newest score is the one in force.
    expect(await screen.findByTestId('human-score-latest')).toHaveTextContent('Scored 4 by reviewer 4: warm');
    expect(screen.getByTestId('human-score-save-11-7')).toHaveTextContent('Re-score');
  });

  it('says a human-scored result is scored even without the right to read the score history', async () => {
    mockPermissions([PERMISSIONS.evaluation.runRead]);
    mockReads({
      results: [{ ...pendingResult(), status: 'ok', native_score: 4, normalized_score: 75, verdict: { score: 4, scored_by: 'human' } }],
    });
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    expect(await screen.findByTestId('human-score-latest')).toHaveTextContent('Scored by a reviewer.');
    expect(screen.getByTestId('human-score-11-7')).not.toHaveTextContent('Waiting');
  });

  it('shows the score but no control to someone who may only read', async () => {
    mockPermissions(READ);
    mockReads({
      scores: [{ id: '1', run_id: '9', dataset_case_id: '11', dimension_id: '7', native_score: 4, normalized_score: 75, note: '', reviewer_id: 3 }],
    });
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));

    expect(await screen.findByTestId('human-score-latest')).toBeInTheDocument();
    expect(screen.queryByTestId('human-score-input-11-7')).not.toBeInTheDocument();
  });

  it('says the server refusal in the cell', async () => {
    mockPermissions(WRITE);
    mockReads();
    server.use(
      http.post(`${BASE}/elitea_core/eval_human_scores/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({ error: 'that result was scored by the judge, not by a human' }, { status: 409 }),
      ),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('evaluation-run-open-9'));
    await userEvent.type(await screen.findByTestId('human-score-input-11-7'), '3');
    await userEvent.click(screen.getByTestId('human-score-save-11-7'));

    expect(await screen.findByText('that result was scored by the judge, not by a human')).toBeInTheDocument();
  });
});

describe('run limits', () => {
  const suite = {
    id: '4',
    name: 'Cheap',
    token_limit_per_case: 2000,
    cost_limit_per_case: null,
    token_limit_per_run: null,
    cost_limit_per_run: 1.5,
    on_run_limit: 'report',
  };

  it('creates run limits with only the controls that do something', async () => {
    mockPermissions(WRITE);
    mockReads({ runs: [] });
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_suites/prompt_lib/:projectId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...suite, ...(sent as object), id: '5' }, { status: 201 });
      }),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('run-limits-toggle'));
    const panel = screen.getByTestId('run-suites-panel');
    // Steps, time and tool output are not offered for the default mode: a case
    // is one model call there (SuiteRuntimeMode.test.tsx covers the other mode).
    expect(panel).not.toHaveTextContent(/steps|time limit|tool output/i);

    await userEvent.type(screen.getByTestId('suite-name'), 'Nightly');
    await userEvent.type(screen.getByTestId('suite-tokens-per-run'), '50000');
    await userEvent.type(screen.getByTestId('suite-cost-per-case'), '0.25');
    await userEvent.click(within(panel).getByRole('radio', { name: 'Only report it' }));
    await userEvent.click(screen.getByTestId('suite-save'));

    await waitFor(() =>
      expect(sent).toEqual({
        name: 'Nightly',
        execution_mode: 'predict_blocking',
        step_limit_per_case: null,
        time_limit_per_case_seconds: null,
        store_tool_outputs: true,
        token_limit_per_case: null,
        cost_limit_per_case: 0.25,
        token_limit_per_run: 50000,
        cost_limit_per_run: null,
        on_run_limit: 'report',
      }),
    );
  });

  it('refuses a bad limit before the request and says why', async () => {
    mockPermissions(WRITE);
    mockReads({ runs: [] });
    renderView();

    await userEvent.click(await screen.findByTestId('run-limits-toggle'));
    await userEvent.type(screen.getByTestId('suite-name'), 'x');
    await userEvent.type(screen.getByTestId('suite-tokens-per-run'), '0');

    expect(screen.getByTestId('suite-error')).toHaveTextContent('token limit');
    expect(screen.getByTestId('suite-save')).toBeDisabled();
  });

  it('is read-only without the right to start runs', async () => {
    mockPermissions(READ);
    mockReads({ runs: [], suites: [suite] });
    renderView();

    await userEvent.click(await screen.findByTestId('run-limits-toggle'));

    expect(screen.queryByTestId('suite-save')).not.toBeInTheDocument();
    expect(screen.getByTestId('suite-name')).toBeDisabled();
  });

  it('starts a run with the chosen limits', async () => {
    mockPermissions(WRITE);
    mockReads({ runs: [], suites: [suite] });
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [{ id: '5', name: 'Support', description: '', application_id: null, is_shared: false, case_count: 2, active_case_count: 2 }],
          total: 1,
        }),
      ),
      http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [
            {
              id: '3', name: 'Helpfulness', description: '', tier: 'project', application_id: null,
              allowed_engines: ['ai'], scale_type: 'ordinal', scale_min: 1, scale_max: 5, polarity: 'higher_better',
              default_weight: 1, default_target: null, default_target_operator: '', code: '', return_contract: '',
            },
          ],
          total: 1,
        }),
      ),
    );
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(run({ status: 'created' }), { status: 201 });
      }),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('run-dimension-3'));
    const selects = screen.getAllByRole('combobox');
    await userEvent.click(selects[0] as HTMLElement);
    await userEvent.click(await screen.findByRole('option', { name: 'Support' }));
    await userEvent.click(screen.getAllByRole('combobox')[1] as HTMLElement);
    await userEvent.click(await screen.findByRole('option', { name: 'Cheap' }));
    await userEvent.click(screen.getByTestId('run-start'));

    await waitFor(() =>
      expect(sent).toMatchObject({ dataset_id: '5', dimension_ids: ['3'], suite_id: '4' }),
    );
  });

  it('forgets a chosen suite that was deleted and starts without it', async () => {
    mockPermissions(WRITE);
    let suites: unknown[] = [suite];
    mockReads({ runs: [] });
    server.use(
      http.get(`${BASE}/elitea_core/eval_suites/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: suites, total: suites.length }),
      ),
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [{ id: '5', name: 'Support', description: '', application_id: null, is_shared: false, case_count: 2, active_case_count: 2 }],
          total: 1,
        }),
      ),
      http.get(`${BASE}/elitea_core/eval_dimensions/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          rows: [
            {
              id: '3', name: 'Helpfulness', description: '', tier: 'project', application_id: null,
              allowed_engines: ['ai'], scale_type: 'ordinal', scale_min: 1, scale_max: 5, polarity: 'higher_better',
              default_weight: 1, default_target: null, default_target_operator: '', code: '', return_contract: '',
            },
          ],
          total: 1,
        }),
      ),
      http.delete(`${BASE}/elitea_core/eval_suite/prompt_lib/:projectId/:suiteId`, () => {
        suites = [];
        return new HttpResponse(null, { status: 204 });
      }),
    );
    let sent: Record<string, unknown> = {};
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(run({ status: 'created' }), { status: 201 });
      }),
    );
    renderView();

    await userEvent.click(await screen.findByTestId('run-dimension-3'));
    await userEvent.click(screen.getAllByRole('combobox')[0] as HTMLElement);
    await userEvent.click(await screen.findByRole('option', { name: 'Support' }));
    await userEvent.click(screen.getAllByRole('combobox')[1] as HTMLElement);
    await userEvent.click(await screen.findByRole('option', { name: 'Cheap' }));

    // Delete the chosen suite in the limits editor.
    await userEvent.click(screen.getByTestId('run-limits-toggle'));
    const panel = screen.getByTestId('run-suites-panel');
    await userEvent.click(within(panel).getAllByRole('combobox')[0] as HTMLElement);
    await userEvent.click(await screen.findByRole('option', { name: 'Cheap' }));
    await userEvent.click(screen.getByTestId('suite-delete'));
    await waitFor(() => expect(screen.queryByTestId('run-suite-select')).not.toBeInTheDocument());

    await userEvent.click(screen.getByTestId('run-start'));
    await waitFor(() => expect(sent['dataset_id']).toBe('5'));
    expect(sent).not.toHaveProperty('suite_id');
  });

  it('says which limit a run reached', async () => {
    mockPermissions(READ);
    mockReads({ runs: [run({ status: 'errored', limit_reached: 'tokens', error: 'the run stopped' })] });
    renderView();

    expect(await screen.findByTestId('evaluation-run-limit-9')).toHaveTextContent('Token limit reached');
  });
});
