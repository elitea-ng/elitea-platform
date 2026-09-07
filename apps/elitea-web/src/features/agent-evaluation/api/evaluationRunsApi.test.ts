import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  addCase,
  cancelRun,
  createDataset,
  editCase,
  fetchEvalDataset,
  fetchEvalDatasets,
  fetchEvalRun,
  fetchEvalRuns,
  fetchScorecard,
  removeCase,
  removeDataset,
  renameDataset,
  startRun,
} from './evaluationRunsApi';

const BASE = '/api/v2';

const dataset = {
  id: '5',
  uuid: 'd-5',
  name: 'Support answers',
  description: 'the ones people ask',
  application_id: null,
  is_shared: false,
  case_count: 2,
};

const run = {
  id: '9',
  dataset_id: '5',
  status: 'finished',
  execution_mode: 'predict_blocking',
  progress: { done: 2, total: 2 },
  headline_score: 75,
};

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('evaluation dataset and run API', () => {
  /*
   * THE ENVELOPE. `eliteaFetch` resolves `{data, status, headers}`, so a caller
   * that types the result as the BODY reads `undefined` for every field on a
   * perfectly good 200 (#132, which shipped twice on one endpoint). Every
   * assertion below reads a real field, which is what makes the unwrap the
   * subject rather than the status code.
   */
  it('unwraps the transport envelope and reads `rows` from the dataset listing', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [dataset], total: 1 }),
      ),
    );
    const datasets = await fetchEvalDatasets('1');
    expect(datasets).toHaveLength(1);
    expect(datasets[0]?.name).toBe('Support answers');
    expect(datasets[0]?.case_count).toBe(2);
  });

  it('sends `agent_id` only when an agent is named', async () => {
    let seen: string | null = null;
    server.use(
      http.get(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, ({ request }) => {
        seen = new URL(request.url).searchParams.get('agent_id');
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );

    await fetchEvalDatasets('1');
    expect(seen).toBeNull();

    await fetchEvalDatasets('1', 42);
    expect(seen).toBe('42');
  });

  /*
   * The detail read asks for a BOUNDED page. A read with no limit is the thing
   * that becomes a problem silently the day the server's case cap is raised.
   */
  it('reads one dataset with a bounded page of cases', async () => {
    let limit: string | null = null;
    server.use(
      http.get(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, ({ request }) => {
        limit = new URL(request.url).searchParams.get('limit');
        return HttpResponse.json({
          ...dataset,
          cases: [{ id: '11', dataset_id: '5', input: 'q', variables: {}, expected_output: null, source_type: 'manual', order_index: 0 }],
          cases_truncated: false,
        });
      }),
    );
    const detail = await fetchEvalDataset('1', '5');
    expect(limit).toBe('200');
    expect(detail.cases).toHaveLength(1);
    expect(detail.cases[0]?.expected_output).toBeNull();
  });

  it('creates a dataset and returns the STORED row, not the request body', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/eval_datasets/prompt_lib/:projectId`, async ({ request }) => {
        const body = (await request.json()) as { name: string };
        // The server applies defaults; a client that echoed its own body back
        // would report success for whatever the database silently altered.
        return HttpResponse.json({ ...dataset, name: body.name, case_count: 0 }, { status: 201 });
      }),
    );
    const created = await createDataset('1', {
      name: 'New',
      description: '',
      application_id: null,
      is_shared: false,
    });
    expect(created.name).toBe('New');
    expect(created.case_count).toBe(0);
  });

  it('renames a dataset through the PUT route', async () => {
    let method = '';
    server.use(
      http.put(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, ({ request }) => {
        method = request.method;
        return HttpResponse.json({ ...dataset, name: 'Renamed' });
      }),
    );
    const renamed = await renameDataset('1', '5', {
      name: 'Renamed',
      description: '',
      application_id: null,
      is_shared: false,
    });
    expect(method).toBe('PUT');
    expect(renamed.name).toBe('Renamed');
  });

  it('deletes a dataset', async () => {
    let path = '';
    server.use(
      http.delete(`${BASE}/elitea_core/eval_dataset/prompt_lib/:projectId/:datasetId`, ({ request }) => {
        path = new URL(request.url).pathname;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await removeDataset('1', '5');
    expect(path).toBe(`${BASE}/elitea_core/eval_dataset/prompt_lib/1/5`);
  });

  /*
   * `expected_output: null` and `expected_output: ''` are DIFFERENT
   * instructions to a judge: the second tells it to mark every non-empty answer
   * wrong. The client must be able to send both.
   */
  it('adds a case and carries a null expected output through unchanged', async () => {
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_dataset_cases/prompt_lib/:projectId/:datasetId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(
          { id: '11', dataset_id: '5', input: 'q', variables: {}, expected_output: null, source_type: 'manual', order_index: 0 },
          { status: 201 },
        );
      }),
    );
    const added = await addCase('1', '5', { input: 'q', variables: {}, expected_output: null });
    expect(sent).toEqual({ input: 'q', variables: {}, expected_output: null });
    expect(added.expected_output).toBeNull();
  });

  it('edits and removes a case through its own dataset path', async () => {
    let editPath = '';
    let deletePath = '';
    server.use(
      http.put(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, ({ request }) => {
        editPath = new URL(request.url).pathname;
        return HttpResponse.json({ id: '11', dataset_id: '5', input: 'edited', variables: {}, expected_output: null, source_type: 'manual', order_index: 0 });
      }),
      http.delete(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, ({ request }) => {
        deletePath = new URL(request.url).pathname;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const edited = await editCase('1', '5', '11', { input: 'edited', variables: {} });
    await removeCase('1', '5', '11');
    expect(editPath).toBe(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/1/5/11`);
    expect(deletePath).toBe(`${BASE}/elitea_core/eval_dataset_case/prompt_lib/1/5/11`);
    expect(edited.input).toBe('edited');
  });

  it('lists runs and reads the `rows` envelope', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [run], total: 1 }),
      ),
    );
    const runs = await fetchEvalRuns('1', 42);
    expect(runs[0]?.status).toBe('finished');
    expect(runs[0]?.progress.done).toBe(2);
  });

  it('starts a run and sends the body the server documents', async () => {
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...run, status: 'created' }, { status: 201 });
      }),
    );
    const started = await startRun('1', {
      dataset_id: '5',
      application_id: 42,
      application_version_id: 9,
      dimension_ids: ['3'],
      trigger_type: 'on_demand',
    });
    expect(sent).toMatchObject({ dataset_id: '5', application_version_id: 9, dimension_ids: ['3'] });
    expect(started.status).toBe('created');
  });

  it('reads one run and cancels one run', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/eval_run/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({ ...run, status: 'running', progress: { done: 1, total: 2 } }),
      ),
      http.post(`${BASE}/elitea_core/eval_run_cancel/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({ ...run, status: 'cancelled' }),
      ),
    );
    expect((await fetchEvalRun('1', '9')).progress.done).toBe(1);
    expect((await cancelRun('1', '9')).status).toBe('cancelled');
  });

  /*
   * The scorecard OMITS `human_scores`. The client must not invent one: a `[]`
   * here would tell a reader there are none, where the truth is that this
   * deployment cannot have any.
   */
  it('reads a scorecard with its named gaps and no invented human scores', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({
          run,
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
              verdict: { score: 5, reason: 'good' },
              evidence: { input: 'q', output: 'a' },
            },
          ],
          headline_score: 100,
          total: 1,
          offset: 0,
          unavailable: [{ key: 'human_scores', reason: 'no human-score surface in this release' }],
        }),
      ),
    );
    const scorecard = await fetchScorecard('1', '9');
    expect(scorecard.total).toBe(1);
    expect(scorecard.unavailable[0]?.key).toBe('human_scores');
    expect((scorecard as unknown as Record<string, unknown>)['human_scores']).toBeUndefined();
  });

  /*
   * A 501 is a REFUSAL WITH A REASON, and it must reach the caller as an error
   * carrying that reason. Swallowing it into a resolved value is how "this
   * dimension cannot be scored" becomes a run that silently never happened.
   */
  it('surfaces the server’s 501 refusal as an error the caller can show', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'the code engine needs a sandbox elitea-main does not have' }, { status: 501 }),
      ),
    );
    await expect(
      startRun('1', { dataset_id: '5', application_version_id: 9, dimension_ids: ['4'] }),
    ).rejects.toBeDefined();
  });
});
