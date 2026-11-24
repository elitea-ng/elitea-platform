import { useState } from 'react';
import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../../__tests__/testUtils';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineNode } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import { HttpActionSettings } from './HttpActionSettings';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  cleanup();
  resetGeneratedClient();
});

describe('HttpActionSettings', () => {
  it('calls listHttpActionRuleNames (names & enabled only) and warns when selected rule is disabled', async () => {
    let requestedUrl = '';
    server.use(
      http.get(`${BASE}/httpactions/prompt_lib/42/rule-names`, ({ request }) => {
        requestedUrl = request.url;
        return HttpResponse.json({
          items: [
            { name: 'fx_reference.rates', enabled: true },
            { name: 'billing.disabled_op', enabled: false },
          ],
        });
      }),
    );

    const node: YamlPipelineNode = {
      id: 'fetch_fx',
      type: 'http',
      revision: 2,
      operation: 'billing.disabled_op',
      request: {},
      response: { mode: 'json', accepted_statuses: [{ first: 200, last: 299 }], max_bytes: 1_048_576 },
      output: ['fx'],
      transition: 'END',
    };

    const ctx = buildFlowEditorContextValue({
      yamlJsonObject: {
        state: {
          run_id: { type: 'str' },
          payload: { type: 'dict' },
          fx: { type: 'dict' },
        },
        entry_point: 'fetch_fx',
        nodes: [node],
      },
    });

    renderWithRouterAndProject(
      <FlowEditorContext.Provider value={ctx}>
        <HttpActionSettings node={node} disabled={false} change={vi.fn()} remove={vi.fn()} />
      </FlowEditorContext.Provider>,
      '42',
    );

    expect(await screen.findByTestId('pipelines-http-action-settings')).toBeTruthy();
    await waitFor(() => {
      expect(screen.getByText(/currently disabled in project settings/i)).toBeTruthy();
    });
    expect(requestedUrl).toContain('/httpactions/prompt_lib/42/rule-names');
    expect(screen.queryByLabelText(/url|origin|credential|secret/i)).toBeNull();
  });

  it('keeps newly added path, query, and header rows visible and emits request bindings on edit', async () => {
    const user = userEvent.setup();
    server.use(
      http.get(`${BASE}/httpactions/prompt_lib/42/rule-names`, () =>
        HttpResponse.json({ items: [{ name: 'fx_reference.rates', enabled: true }] }),
      ),
    );

    const change = vi.fn();
    const baseNode: YamlPipelineNode = {
      id: 'fetch_fx',
      type: 'http',
      revision: 2,
      operation: 'fx_reference.rates',
      request: {},
      response: { mode: 'artifact', accepted_statuses: [{ first: 200, last: 200 }], max_bytes: 16_384 },
      output: ['fx'],
      transition: 'END',
    };
    const ctx = buildFlowEditorContextValue({
      yamlJsonObject: {
        state: { run_id: { type: 'str' }, fx: { type: 'dict' } },
        entry_point: 'fetch_fx',
        nodes: [baseNode],
      },
    });

    function Harness() {
      const [node, setNode] = useState<YamlPipelineNode>(baseNode);
      return (
        <FlowEditorContext.Provider value={ctx}>
          <button
            type="button"
            data-testid="pipelines-http-test-swap-statuses"
            onClick={() =>
              setNode({
                ...node,
                response: { mode: 'artifact', accepted_statuses: [{ first: 200, last: 201 }], max_bytes: 16_384 },
              })
            }
          >
            swap
          </button>
          <HttpActionSettings node={node} disabled={false} change={change} remove={vi.fn()} />
        </FlowEditorContext.Provider>
      );
    }

    renderWithRouterAndProject(<Harness />, '42');

    expect(await screen.findByTestId('pipelines-http-action-settings')).toBeTruthy();

    // Clicking Add persists an editable draft row for path, query, and headers.
    await user.click(screen.getByTestId('pipelines-http-path-add'));
    const pathNameInput = screen.getByLabelText('Name');
    fireEvent.change(pathNameInput, { target: { value: 'run_id' } });
    expect(change).toHaveBeenCalledWith('request', { path: { run_id: { from: '' } } });

    await user.click(screen.getByTestId('pipelines-http-query-add'));
    const queryNameInputs = screen.getAllByLabelText('Name');
    fireEvent.change(queryNameInputs[queryNameInputs.length - 1]!, { target: { value: 'limit' } });
    expect(change).toHaveBeenCalledWith('request', { query: { limit: { from: '' } } });

    await user.click(screen.getByTestId('pipelines-http-headers-add'));
    const headerNameInput = screen.getByLabelText('Header name');
    fireEvent.change(headerNameInput, { target: { value: 'X-Trace' } });
    expect(change).toHaveBeenCalledWith('request', { headers: [{ name: 'X-Trace', from: '' }] });

    // External update to accepted_statuses syncs the draft input.
    const statusInput = screen.getByLabelText<HTMLInputElement>('Accepted HTTP statuses');
    expect(statusInput.value).toBe('200');
    await user.click(screen.getByTestId('pipelines-http-test-swap-statuses'));
    expect(screen.getByLabelText<HTMLInputElement>('Accepted HTTP statuses').value).toBe('200-201');
  });
});
