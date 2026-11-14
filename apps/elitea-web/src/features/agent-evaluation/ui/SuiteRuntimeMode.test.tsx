import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { EvalSuite } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { RunSuitesPanel } from './RunSuitesPanel';

const BASE = '/api/v2';

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

const agentSuite: EvalSuite = {
  id: '8',
  name: 'Agent runs',
  on_run_limit: 'stop',
  execution_mode: 'runtime_plane',
  step_limit_per_case: 12,
  time_limit_per_case_seconds: 90,
  store_tool_outputs: false,
};

async function chooseMode(label: string): Promise<void> {
  await userEvent.click(screen.getByRole('combobox', { name: 'How a case runs' }));
  await userEvent.click(await screen.findByRole('option', { name: label }));
}

describe('the suite editor and the execution mode (issue 2013, step 2)', () => {
  // A case of the default mode is one model call: a steps limit or a tool
  // outputs setting would change nothing, so none is on screen.
  it('shows the steps, time and tool output controls only for the agent-run mode', async () => {
    renderWithEvaluationProviders(<RunSuitesPanel projectId="1" suites={[]} canEdit />);

    expect(screen.getByRole('combobox', { name: 'How a case runs' })).toHaveTextContent(/one model call/i);
    expect(screen.queryByTestId('suite-steps-per-case')).not.toBeInTheDocument();
    expect(screen.queryByTestId('suite-time-per-case')).not.toBeInTheDocument();
    expect(screen.queryByTestId('suite-store-tool-outputs')).not.toBeInTheDocument();

    await chooseMode('Agent run (tools, steps and trajectory)');

    expect(screen.getByTestId('suite-steps-per-case')).toBeInTheDocument();
    expect(screen.getByTestId('suite-time-per-case')).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Store tool outputs in the results' })).toBeChecked();
    expect(screen.getByTestId('suite-runtime-note')).toHaveTextContent(/approval/i);

    await chooseMode('One model call (instructions and model only)');
    expect(screen.queryByTestId('suite-steps-per-case')).not.toBeInTheDocument();
  });

  it('saves the agent-run controls', async () => {
    let sent: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/eval_suites/prompt_lib/:projectId`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...agentSuite, ...(sent as object), id: '9' }, { status: 201 });
      }),
      http.get(`${BASE}/elitea_core/eval_suites/prompt_lib/:projectId`, () => HttpResponse.json({ rows: [], total: 0 })),
    );
    renderWithEvaluationProviders(<RunSuitesPanel projectId="1" suites={[]} canEdit />);

    await userEvent.type(screen.getByTestId('suite-name'), 'Agent runs');
    await chooseMode('Agent run (tools, steps and trajectory)');
    await userEvent.type(screen.getByTestId('suite-steps-per-case'), '12');
    await userEvent.type(screen.getByTestId('suite-time-per-case'), '90');
    await userEvent.click(screen.getByRole('checkbox', { name: 'Store tool outputs in the results' }));
    await userEvent.click(screen.getByTestId('suite-save'));

    await waitFor(() =>
      expect(sent).toMatchObject({
        name: 'Agent runs',
        execution_mode: 'runtime_plane',
        step_limit_per_case: 12,
        time_limit_per_case_seconds: 90,
        store_tool_outputs: false,
      }),
    );
  });

  it('refuses a bad steps limit before the request', async () => {
    renderWithEvaluationProviders(<RunSuitesPanel projectId="1" suites={[]} canEdit />);

    await userEvent.type(screen.getByTestId('suite-name'), 'x');
    await chooseMode('Agent run (tools, steps and trajectory)');
    await userEvent.type(screen.getByTestId('suite-steps-per-case'), '0');

    expect(screen.getByTestId('suite-error')).toHaveTextContent(/steps limit/i);
    expect(screen.getByTestId('suite-save')).toBeDisabled();
  });

  it('opens a stored agent-run suite with its controls', async () => {
    renderWithEvaluationProviders(<RunSuitesPanel projectId="1" suites={[agentSuite]} canEdit />);

    await userEvent.click(screen.getByRole('combobox', { name: 'Saved limits' }));
    await userEvent.click(await screen.findByRole('option', { name: 'Agent runs' }));

    expect(screen.getByTestId('suite-steps-per-case')).toHaveValue('12');
    expect(screen.getByTestId('suite-time-per-case')).toHaveValue('90');
    expect(screen.getByRole('checkbox', { name: 'Store tool outputs in the results' })).not.toBeChecked();
  });
});
