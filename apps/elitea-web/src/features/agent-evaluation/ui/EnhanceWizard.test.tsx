import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EnhanceWizard } from './EnhanceWizard';

const BASE = '/api/v2';
const LIFECYCLE = `${BASE}/elitea_core`;

const proposal = {
  run_id: '9',
  application_id: 42,
  application_version_id: 7,
  summary: 'The agent answers vaguely.',
  analysed_rows: 2,
  missed_rows: 2,
  instruction_patch: { rationale: 'Be concrete.', instructions: 'be helpful\nand concrete' },
  dimension_changes: [
    { dimension_id: '3', dimension_name: 'Helpfulness', description: 'Does it give a concrete answer?', default_target: 3, default_target_operator: '>=', rationale: 'Rubric was loose.' },
  ],
  dataset_changes: [
    { case_id: '11', dataset_id: '5', input: 'what is Go?', current_expected_output: 'old', expected_output: 'Go is a compiled language', rationale: 'Too narrow.' },
  ],
};

interface Recorded {
  versions: Record<string, unknown>[];
  dimensions: Record<string, unknown>[];
  cases: Record<string, unknown>[];
  publishCalls: string[];
}

function mockServer(options: { readonly versionStatus?: number } = {}): Recorded {
  const recorded: Recorded = { versions: [], dimensions: [], cases: [], publishCalls: [] };
  server.use(
    http.post(`${LIFECYCLE}/eval_run_enhance/prompt_lib/:projectId/:runId`, () => HttpResponse.json(proposal)),
    http.get(`${LIFECYCLE}/version/prompt_lib/:projectId/:applicationId/:versionId`, () =>
      HttpResponse.json({ id: '7', application_id: '42', name: 'base', status: 'draft', instructions: 'be helpful', agent_type: 'openai', llm_settings: { model_name: 'qwen' } }),
    ),
    http.get(`${LIFECYCLE}/agent_registry/prompt_lib/:projectId/:applicationId`, () => HttpResponse.json({ risk_tier: 'medium' })),
    http.get(`${LIFECYCLE}/eval_dimensions/prompt_lib/:projectId`, () =>
      HttpResponse.json({
        rows: [{ id: '3', name: 'Helpfulness', description: 'old rubric', tier: 'project', application_id: null, allowed_engines: ['ai'], scale_type: 'ordinal', scale_min: 1, scale_max: 5, polarity: 'higher_better', default_weight: 2, default_target: 4, default_target_operator: '>=', code: '', return_contract: '' }],
        total: 1,
      }),
    ),
    http.get(`${LIFECYCLE}/eval_dataset/prompt_lib/:projectId/:datasetId`, () =>
      HttpResponse.json({ id: '5', name: 'ds', cases: [{ id: '11', dataset_id: '5', input: 'what is {{x}}?', variables: { x: 'Go' }, expected_output: 'old', excluded: false }] }),
    ),
    http.post(`${LIFECYCLE}/versions/prompt_lib/:projectId/:applicationId`, async ({ request }) => {
      recorded.versions.push((await request.json()) as Record<string, unknown>);
      if (options.versionStatus !== undefined) return HttpResponse.json({ error: 'version create is not permitted' }, { status: options.versionStatus });
      return HttpResponse.json({ id: '8', application_id: '42', name: 'eval-fix-9', status: 'draft' }, { status: 201 });
    }),
    http.put(`${LIFECYCLE}/eval_dimension/prompt_lib/:projectId/:dimensionId`, async ({ request }) => {
      recorded.dimensions.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ id: '3' });
    }),
    http.put(`${LIFECYCLE}/eval_dataset_case/prompt_lib/:projectId/:datasetId/:caseId`, async ({ request }) => {
      recorded.cases.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ id: '11' });
    }),
    // Publishing is the agent's release path. The wizard must never call it.
    http.post(`${LIFECYCLE}/publish/*`, () => {
      recorded.publishCalls.push('publish');
      return HttpResponse.json({}, { status: 500 });
    }),
  );
  return recorded;
}

async function toReview(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  for (let step = 0; step < 3; step += 1) {
    await user.click(await screen.findByTestId('enhance-next'));
  }
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('EnhanceWizard', () => {
  it('walks the three proposals and saves each accepted fix through its own route', async () => {
    const recorded = mockServer();
    const user = userEvent.setup();
    renderWithEvaluationProviders(<EnhanceWizard projectId="1" runId="9" onClose={() => undefined} />);

    expect(await screen.findByTestId('enhance-summary')).toHaveTextContent('The agent answers vaguely.');
    // Step 1: the patch is shown against the PINNED version's instructions.
    expect(await screen.findByTestId('enhance-instruction-diff')).toHaveTextContent('and concrete');
    await user.click(screen.getByTestId('enhance-next'));
    expect(screen.getByTestId('enhance-dimension-3')).toHaveTextContent('New target: >= 3');
    await user.click(screen.getByTestId('enhance-next'));
    expect(screen.getByTestId('enhance-case-11')).toHaveTextContent('Too narrow.');
    await user.click(screen.getByTestId('enhance-next'));
    expect(screen.getByTestId('enhance-review-summary')).toHaveTextContent('1 instruction draft, 1 dimension change(s), 1 expected-output change(s)');

    await user.click(screen.getByTestId('enhance-save'));
    expect(await screen.findByTestId('enhance-draft-created')).toHaveTextContent('Draft version eval-fix-9 created. It is not published.');

    // A NEW VERSION built from the pinned one, as a draft with copied skills.
    expect(recorded.versions).toHaveLength(1);
    expect(recorded.versions[0]).toMatchObject({
      name: 'eval-fix-9', instructions: 'be helpful\nand concrete', agent_type: 'openai',
      llm_settings: { model_name: 'qwen' }, copy_skills_from_version_id: 7,
    });
    // The dimension keeps its stored weight and scale; only rubric and target move.
    expect(recorded.dimensions[0]).toMatchObject({ description: 'Does it give a concrete answer?', default_target: 3, default_target_operator: '>=', default_weight: 2, scale_max: 5 });
    // The case keeps its stored input and variables.
    expect(recorded.cases[0]).toEqual({ input: 'what is {{x}}?', variables: { x: 'Go' }, expected_output: 'Go is a compiled language' });
    // Nothing was published.
    expect(recorded.publishCalls).toEqual([]);
  });

  it('saves only what is still ticked', async () => {
    const recorded = mockServer();
    const user = userEvent.setup();
    renderWithEvaluationProviders(<EnhanceWizard projectId="1" runId="9" onClose={() => undefined} />);

    await user.click(await screen.findByRole('checkbox', { name: /Create a draft version/ }));
    await user.click(screen.getByTestId('enhance-next'));
    await user.click(screen.getByTestId('enhance-next'));
    await user.click(screen.getByTestId('enhance-next'));
    await user.click(screen.getByTestId('enhance-save'));

    await waitFor(() => expect(recorded.cases).toHaveLength(1));
    expect(recorded.versions).toHaveLength(0);
    expect(recorded.dimensions).toHaveLength(1);
  });

  // A refused version write must not hide that the other fixes were saved.
  it('reports a refused instruction patch and still saves the others', async () => {
    const recorded = mockServer({ versionStatus: 403 });
    const user = userEvent.setup();
    renderWithEvaluationProviders(<EnhanceWizard projectId="1" runId="9" onClose={() => undefined} />);
    await screen.findByTestId('enhance-summary');
    await toReview(user);
    await user.click(screen.getByTestId('enhance-save'));

    expect(await screen.findByTestId('enhance-applied-instructions-instructions')).toHaveTextContent('was not saved: version create is not permitted');
    expect(screen.getByTestId('enhance-applied-dimension-Helpfulness')).toHaveTextContent('saved');
    expect(recorded.dimensions).toHaveLength(1);
    expect(recorded.cases).toHaveLength(1);
    expect(screen.queryByTestId('enhance-request-publish')).toBeNull();
  });

  it('sends the new draft to the change request dialog, not to publish', async () => {
    const recorded = mockServer();
    const user = userEvent.setup();
    renderWithEvaluationProviders(<EnhanceWizard projectId="1" runId="9" onClose={() => undefined} />);
    await screen.findByTestId('enhance-summary');
    await toReview(user);
    await user.click(screen.getByTestId('enhance-save'));
    await user.click(await screen.findByTestId('enhance-request-publish'));

    const dialog = await screen.findByTestId('request-publish-dialog');
    expect(within(dialog).getByTestId('request-publish-submit')).toBeInTheDocument();
    expect(recorded.publishCalls).toEqual([]);
  });

  it('names why the model could not propose', async () => {
    server.use(
      http.post(`${LIFECYCLE}/eval_run_enhance/prompt_lib/:projectId/:runId`, () =>
        HttpResponse.json({ error: 'no scored case missed its target, so there is nothing to enhance' }, { status: 422 }),
      ),
    );
    renderWithEvaluationProviders(<EnhanceWizard projectId="1" runId="9" onClose={() => undefined} />);
    expect(await screen.findByTestId('enhance-error')).toHaveTextContent('nothing to enhance');
    expect(screen.getByTestId('enhance-next')).toBeDisabled();
  });
});
