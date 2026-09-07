/**
 * THE COMPOSITION ROOT for Agent Evaluation, exercised through the REAL page.
 *
 * Every other test in this feature mounts one component with props a test file
 * wrote. This one mounts `EditApplication` on its real route and clicks through
 * to the scorecard, which is the only place four separate facts are checked at
 * once:
 *
 *   1. the Evaluation tab is registered on the agent editor at all;
 *   2. the three sub-views exist and are reachable;
 *   3. the RUN sub-view receives the version the editor has open — a prop that
 *      is correct in the feature and correct in the page and wired in neither
 *      is the shape #597 records, which 2475 green unit tests could not see;
 *   4. a scorecard renders from the routes the server actually serves.
 *
 * A unit suite is blind to (1) and (3) by construction: both halves compile,
 * both halves are tested, and the composition between them is the defect.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { EditApplication } from './EditApplication';
import { renderAgentsRoute } from './__tests__/testRouter';

installCodeMirrorTestPolyfills();

const CATALOGUE = {
  items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }],
  default_model_name: 'gpt-4o',
};

function detail() {
  return {
    id: '42',
    name: 'My Agent',
    description: 'A helpful agent',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [{ id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' }],
    version_details: {
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
      instructions: 'Be helpful.',
      conversation_starters: [],
    },
  };
}

const EVALUATION_PERMISSIONS = [
  PERMISSIONS.evaluation.dimensionRead,
  PERMISSIONS.evaluation.datasetRead,
  PERMISSIONS.evaluation.datasetCreate,
  PERMISSIONS.evaluation.datasetUpdate,
  PERMISSIONS.evaluation.runRead,
  PERMISSIONS.evaluation.runCreate,
];

const RUN = {
  id: '9',
  dataset_id: '5',
  status: 'finished',
  execution_mode: 'predict_blocking',
  progress: { done: 1, total: 1 },
  headline_score: 100,
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getGetApplicationMockHandler(detail()),
    http.get('*/configurations/models/:projectId', () => HttpResponse.json(CATALOGUE)),
    http.get('*/social/author*', () => HttpResponse.json({ id: '6', name: 'Driver', avatar: '' })),
    http.get('*/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json(EVALUATION_PERMISSIONS.map((name) => ({ name, enabled: true }))),
    ),
    http.get('*/elitea_core/eval_dimensions/prompt_lib/:projectId', () =>
      HttpResponse.json({ rows: [], total: 0 }),
    ),
    http.get('*/elitea_core/eval_datasets/prompt_lib/:projectId', () =>
      HttpResponse.json({
        rows: [
          {
            id: '5',
            name: 'Support answers',
            description: '',
            application_id: 42,
            is_shared: false,
            case_count: 1,
          },
        ],
        total: 1,
      }),
    ),
    http.get('*/elitea_core/eval_runs/prompt_lib/:projectId', () =>
      HttpResponse.json({ rows: [RUN], total: 1 }),
    ),
    http.get('*/elitea_core/eval_results/prompt_lib/:projectId/:runId', () =>
      HttpResponse.json({
        run: RUN,
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
        ],
        headline_score: 100,
        total: 1,
        offset: 0,
        unavailable: [{ key: 'human_scores', reason: 'this release has no human-score surface' }],
      }),
    ),
  );
});

afterEach(() => resetGeneratedClient());

describe('EditApplication — the Evaluation tab', () => {
  it('mounts the three evaluation sub-views on the real agent editor route', async () => {
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await userEvent.click(await screen.findByTestId('edit-application-tab-evaluation', {}, { timeout: 5_000 }));

    const panel = await screen.findByTestId('evaluation-panel', {}, { timeout: 5_000 });
    expect(panel).toContainElement(screen.getByTestId('evaluation-view-library'));
    expect(panel).toContainElement(screen.getByTestId('evaluation-view-datasets'));
    expect(panel).toContainElement(screen.getByTestId('evaluation-view-runs'));
  }, 20_000);

  it('reaches a dataset through the Datasets sub-view', async () => {
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await userEvent.click(await screen.findByTestId('edit-application-tab-evaluation', {}, { timeout: 5_000 }));
    await userEvent.click(await screen.findByTestId('evaluation-view-datasets', {}, { timeout: 5_000 }));

    expect(await screen.findByText('Support answers', {}, { timeout: 5_000 })).toBeInTheDocument();
  }, 20_000);

  /*
   * THE WIRING TEST. The run start form needs the version the editor has open;
   * if the page does not pass it through, the form refuses with "Save this
   * agent…" and the feature is dark. Nothing in the feature's own unit tests
   * can tell that apart from a genuinely unsaved agent.
   */
  it('gives the run form the version the editor has open', async () => {
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await userEvent.click(await screen.findByTestId('edit-application-tab-evaluation', {}, { timeout: 5_000 }));
    await userEvent.click(await screen.findByTestId('evaluation-view-runs', {}, { timeout: 5_000 }));

    const form = await screen.findByTestId('run-start-form', {}, { timeout: 5_000 });
    await waitFor(() => expect(form).not.toHaveTextContent('Save this agent'));
    // The remaining blocker is the one a person can clear on this screen.
    expect(screen.getByTestId('run-start-error')).toHaveTextContent('Choose a dataset.');
  }, 20_000);

  it('opens a run’s scorecard from the editor', async () => {
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await userEvent.click(await screen.findByTestId('edit-application-tab-evaluation', {}, { timeout: 5_000 }));
    await userEvent.click(await screen.findByTestId('evaluation-view-runs', {}, { timeout: 5_000 }));
    await userEvent.click(await screen.findByTestId('evaluation-run-open-9', {}, { timeout: 5_000 }));

    expect(await screen.findByTestId('scorecard-headline', {}, { timeout: 5_000 })).toHaveTextContent(
      'Overall 100 / 100',
    );
    // WHAT ACTUALLY RAN reaches the screen through the whole stack, so a
    // reader does not take the number for a measurement of the whole agent.
    expect(screen.getByTestId('scorecard-execution-mode')).toHaveTextContent('Tools and toolkits were not used');
    expect(screen.getByTestId('scorecard-unavailable')).toHaveTextContent('no human-score surface');
  }, 20_000);
});
