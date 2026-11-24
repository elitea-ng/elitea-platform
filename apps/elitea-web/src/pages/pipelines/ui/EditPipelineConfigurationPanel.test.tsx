import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { renderPipelinesRoute } from '../__tests__/testRouter';
import { EditPipelineConfigurationPanel } from './EditPipelineConfigurationPanel';

const globals = globalThis as unknown as Record<string, unknown>;

beforeEach(() => {
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/configurations/models/:projectId', () =>
      HttpResponse.json({
        items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }],
        default_model_name: 'gpt-4o',
      }),
    ),
    http.get('*/elitea_core/tags/prompt_lib/:projectId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/applications/prompt_lib/:projectId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/pipeline_triggers/prompt_lib/:projectId/:versionId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/pipeline_trigger/prompt_lib/:projectId/:versionId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/schedule/prompt_lib/:projectId/:versionId', () => HttpResponse.json({ rows: [] })),
    http.get('*/elitea_core/schedules/prompt_lib/:projectId', () => HttpResponse.json({ rows: [] })),
  );
});

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('EditPipelineConfigurationPanel', () => {
  it('orders left config accordions as General -> Tools -> Welcome -> Starters -> Triggers & schedules -> Advanced -> Information and preserves MCP access toggle', async () => {
    renderPipelinesRoute(
      <EditPipelineConfigurationPanel
        projectId="9"
        applicationId={42}
        activeVersion={{ id: '7', name: 'base', status: 'draft', instructions: 'nodes: []' } as never}
        editor={{
          values: {
            name: 'Order Pipeline',
            description: 'Checks section order',
            version_details: {
              id: 7,
              welcome_message: '',
              conversation_starters: [],
              variables: [],
              meta: {},
            },
          } as never,
          onFieldChange: vi.fn(),
        }}
        versionFields={{
          fields: {
            tags: [{ id: 1, name: 'mcp', data: null }],
            tools: [],
            internalTools: [],
            llmSettings: { model_name: 'gpt-4o', model_project_id: '9' },
          },
          setTags: vi.fn(),
          setTools: vi.fn(),
          setInternalTools: vi.fn(),
          applyFieldChange: vi.fn(),
          reset: vi.fn(),
        } as never}
        isEditorDisabled={false}
        isDirty={false}
        isReadOnly={false}
        onModelSettingsChange={vi.fn()}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    const panel = await screen.findByTestId('edit-pipeline-configuration-panel');
    const text = panel.textContent ?? '';
    const positions = [
      text.indexOf('General'),
      text.indexOf('Tools'),
      text.indexOf('Welcome'),
      text.indexOf('Starters') >= 0 ? text.indexOf('Starters') : text.indexOf('starters'),
      text.indexOf('Triggers & schedules'),
      text.indexOf('Advanced'),
      text.indexOf('Information'),
    ];
    for (const pos of positions) {
      expect(pos).toBeGreaterThanOrEqual(0);
    }
    for (let i = 1; i < positions.length; i += 1) {
      expect(positions[i]).toBeGreaterThan(positions[i - 1] as number);
    }
    expect(screen.getByText(/Enable MCP access/i)).toBeInTheDocument();
  });
});
