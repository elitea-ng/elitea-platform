import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { RunScorecard } from './RunScorecard';

const BASE = '/api/v2';
const VERSIONS_GET = 'models.applications.versions.get';

function mock(options: { readonly permissions: readonly string[]; readonly status: string; readonly targetMet: boolean }): void {
  const run = { id: '9', dataset_id: '5', application_id: 42, status: options.status, execution_mode: 'predict_blocking', progress: { done: 1, total: 1 }, headline_score: 40, snapshot: { dimensions: { '3': { name: 'Helpfulness' } } } };
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(options.permissions.map((name) => ({ name, enabled: true }))),
    ),
    http.get(`${BASE}/elitea_core/eval_run/prompt_lib/:projectId/:runId`, () => HttpResponse.json(run)),
    http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, () =>
      HttpResponse.json({
        run, headline_score: 40, total: 1, offset: 0, unavailable: [],
        results: [{ id: 'r1', run_id: '9', dataset_case_id: '11', dimension_id: '3', status: 'ok', native_score: 2, normalized_score: 25, target_met: options.targetMet, evidence: {} }],
      }),
    ),
    http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
    http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () => HttpResponse.json({ rows: [], total: 0 })),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('RunScorecard — Enhance with AI entry point', () => {
  it('offers Enhance on a finished run that missed a target, to someone who may start runs', async () => {
    mock({ permissions: [PERMISSIONS.evaluation.runRead, PERMISSIONS.evaluation.runCreate, VERSIONS_GET], status: 'finished', targetMet: false });
    renderWithEvaluationProviders(<RunScorecard projectId="1" runId="9" onClose={() => undefined} />);
    expect(await screen.findByTestId('run-enhance')).toBeInTheDocument();
    // The insights render in the same dialog.
    expect(await screen.findByTestId('run-efficiency')).toBeInTheDocument();
  });

  it('does not offer it when every target was met', async () => {
    mock({ permissions: [PERMISSIONS.evaluation.runRead, PERMISSIONS.evaluation.runCreate, VERSIONS_GET], status: 'finished', targetMet: true });
    renderWithEvaluationProviders(<RunScorecard projectId="1" runId="9" onClose={() => undefined} />);
    await screen.findByTestId('scorecard-headline');
    expect(screen.queryByTestId('run-enhance')).toBeNull();
  });

  it('does not offer it to a reader, because it spends model tokens', async () => {
    mock({ permissions: [PERMISSIONS.evaluation.runRead], status: 'finished', targetMet: false });
    renderWithEvaluationProviders(<RunScorecard projectId="1" runId="9" onClose={() => undefined} />);
    await screen.findByTestId('scorecard-headline');
    expect(screen.queryByTestId('run-enhance')).toBeNull();
  });

  // The proposal quotes the pinned version's instructions, which the run permissions do not grant.
  it('does not offer it without the version read permission', async () => {
    mock({ permissions: [PERMISSIONS.evaluation.runRead, PERMISSIONS.evaluation.runCreate], status: 'finished', targetMet: false });
    renderWithEvaluationProviders(<RunScorecard projectId="1" runId="9" onClose={() => undefined} />);
    await screen.findByTestId('scorecard-headline');
    expect(screen.queryByTestId('run-enhance')).toBeNull();
  });
});
