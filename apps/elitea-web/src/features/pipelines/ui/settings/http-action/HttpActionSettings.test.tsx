import { cleanup, screen, waitFor } from '@testing-library/react';
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
});
