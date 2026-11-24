import { cleanup } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { authoredExtensionYaml, renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { HttpNode } from './HttpNode';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/httpactions/prompt_lib/*/rules/names`, () =>
      HttpResponse.json({
        items: [{ name: 'fx_reference.rates', enabled: true }],
      }),
    ),
  );
});

afterEach(() => {
  cleanup();
  resetGeneratedClient();
});

const httpNodeYaml = `  - id: extension
    type: http
    revision: 2
    operation: fx_reference.rates
    request:
      path:
        run_id:
          from: run_id
    response:
      mode: json
      accepted_statuses:
        - first: 200
          last: 200
      max_bytes: 16384
    output:
      - fx
    transition: END`;

describe('HttpNode', () => {
  it('renders inside Wave U2 ExtensionNodeShell with HttpActionSettings and Advanced NodeSettings section', async () => {
    const yaml = authoredExtensionYaml(httpNodeYaml);
    const view = renderExtensionCard(HttpNode, yaml);
    expect(await view.findByTestId('pipelines-http-action-settings')).toBeTruthy();
    expect(view.getByRole('button', { name: /advanced/i })).toBeTruthy();
  });
});
