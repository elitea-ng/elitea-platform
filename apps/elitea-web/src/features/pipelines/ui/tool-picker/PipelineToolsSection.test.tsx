import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { toolkitValidation } from '@/entities/toolkit';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import type { PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';
import { PipelineToolCard } from './PipelineToolCard';
import { PipelineToolsSection } from './PipelineToolsSection';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
      HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true }),
    ),
  );
  toolkitValidation.useToolkitValidationStore
    .getState()
    .setToolkitValidationInfo(toolkitValidation.buildToolkitValidationKey(PROJECT_ID, '1'), []);
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('PipelineToolCard & PipelineToolsSection', () => {
  it('toggles exposed tools, clicks Used by node chips, and opens safe URLs', async () => {
    const user = userEvent.setup();
    const onSelectNode = vi.fn();
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null);

    const tool: PickerToolEntry = {
      id: 1,
      type: 'github',
      name: 'GitHub',
      toolkit_name: 'gh',
      settings: { selected_tools: ['list_issues', 'create_pr'] },
    };

    const { findByTestId, queryByTestId } = renderWithRouterAndProject(
      <PipelineToolCard
        tool={tool}
        kind="toolkit"
        usedBy={['node-1']}
        onSelectNode={onSelectNode}
        projectId={PROJECT_ID}
      />,
      PROJECT_ID,
    );

    const toggle = await findByTestId('pipeline-tool-card-show-tools');
    expect(queryByTestId('pipeline-tool-card-tools')).not.toBeInTheDocument();
    await user.click(toggle);
    expect(await findByTestId('pipeline-tool-card-tools')).toHaveTextContent('List issues');
    expect(await findByTestId('pipeline-tool-card-tools')).toHaveTextContent('Create pr');

    await user.click(await findByTestId('pipeline-tool-card-used-by'));
    expect(onSelectNode).toHaveBeenCalledWith('node-1');

    await user.click(await findByTestId('pipeline-tool-card-open'));
    expect(openSpy).toHaveBeenCalledWith(
      `${window.location.origin}/toolkits/all/1`,
      '_blank',
      'noopener,noreferrer',
    );
  });

  it('displays EL-7516 misconfigured-toolkit warning indicator from validation store and override', async () => {
    const key = toolkitValidation.buildToolkitValidationKey(PROJECT_ID, '1');
    toolkitValidation.useToolkitValidationStore
      .getState()
      .setToolkitValidationInfo(key, [{ msg: 'Invalid API token for GitHub toolkit' }]);

    const tool: PickerToolEntry = { id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' };
    const { findByTestId } = renderWithRouterAndProject(
      <PipelineToolCard tool={tool} kind="toolkit" projectId={PROJECT_ID} />,
      PROJECT_ID,
    );

    expect(await findByTestId('pipeline-tool-card-validation-warning')).toHaveTextContent(
      'Invalid API token for GitHub toolkit',
    );

    const { findByText } = renderWithRouterAndProject(
      <PipelineToolCard
        tool={tool}
        kind="toolkit"
        projectId={PROJECT_ID}
        validationOverride={{
          message: 'Private credential required',
          credentialId: 'cred-123',
          isForeignPrivateCredential: true,
        }}
      />,
      PROJECT_ID,
    );
    expect(await findByText('Credential setup required:')).toBeInTheDocument();
  });

  it('PipelineToolsSection shows empty state, groups tools, hides attachmentToolkitId, and allows removing unused tools', async () => {
    const user = userEvent.setup();
    const onRemoveTool = vi.fn();

    const emptyView = renderWithRouterAndProject(
      <PipelineToolsSection tools={[]} entityProjectId={PROJECT_ID} />,
      PROJECT_ID,
    );
    expect(await emptyView.findByTestId('pipeline-tools-section-empty')).toHaveTextContent(
      'Tools you select in nodes will appear here.',
    );

    const usedTool: PickerToolEntry = { id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' };
    const unusedTool: PickerToolEntry = { id: 2, type: 'jira', name: 'Jira', toolkit_name: 'jira' };
    const hiddenAttachmentTool: PickerToolEntry = { id: 99, type: 'artifact', name: 'Attachments' };

    const populatedView = renderWithRouterAndProject(
      <PipelineToolsSection
        tools={[usedTool, unusedTool, hiddenAttachmentTool]}
        yamlJsonObject={{ nodes: [{ id: 'n1', type: 'toolkit', toolkit_name: 'gh' }] }}
        attachmentToolkitId={99}
        onRemoveTool={onRemoveTool}
        entityProjectId={PROJECT_ID}
      />,
      PROJECT_ID,
    );

    await populatedView.findByTestId('pipeline-tools-list');
    expect(populatedView.getAllByTestId('pipeline-tool-card')).toHaveLength(2);
    const removeButtons = populatedView.getAllByTestId('pipeline-tool-card-remove');
    expect(removeButtons).toHaveLength(1);
    await user.click(removeButtons[0]!);
    expect(onRemoveTool).toHaveBeenCalledWith(unusedTool);
  });
});
