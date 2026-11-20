import { describe, expect, it } from 'vitest';

import {
  collectToolReferences,
  findAttachedToolkit,
  getAttachmentKind,
  getAttachmentRefKey,
  getAttachmentRefName,
  getReferencedNames,
  getToolkitDisplayName,
  groupAttachmentsByKind,
  isApplicationTool,
  isMcpAttachment,
} from './pipelineToolUsage.helpers';
import { PICKER_KINDS, TOOL_REF_KINDS } from './toolPicker.constants';
import type { PickerToolEntry } from './toolPicker.types';

describe('pipelineToolUsage.helpers', () => {
  it('distinguishes application, MCP, and standard toolkit attachments', () => {
    const app: PickerToolEntry = { type: 'application', name: 'Helper', agent_type: 'classic' };
    const pipeline: PickerToolEntry = { type: 'application', name: 'SubPipe', agent_type: 'pipeline' };
    const mcpByMeta: PickerToolEntry = { id: 1, type: 'custom', name: 'McpA', meta: { mcp: true } };
    const mcpByType: PickerToolEntry = { id: 2, type: 'mcp', name: 'McpB' };
    const toolkit: PickerToolEntry = { id: 3, type: 'github', name: 'GH', toolkit_name: 'gh_main' };

    expect(isApplicationTool(app)).toBe(true);
    expect(isApplicationTool(toolkit)).toBe(false);
    expect(isMcpAttachment(mcpByMeta)).toBe(true);
    expect(isMcpAttachment(mcpByType)).toBe(true);
    expect(isMcpAttachment(app)).toBe(false);
    expect(isMcpAttachment(toolkit)).toBe(false);

    expect(getAttachmentKind(app)).toBe(PICKER_KINDS.agent);
    expect(getAttachmentKind(pipeline)).toBe(PICKER_KINDS.pipeline);
    expect(getAttachmentKind(mcpByMeta)).toBe(PICKER_KINDS.mcp);
    expect(getAttachmentKind(toolkit)).toBe(PICKER_KINDS.toolkit);
  });

  it('collects toolkit, application, loop_toolkit_name, and tool_names references', () => {
    const refs = collectToolReferences({
      nodes: [
        { id: 'n1', type: 'toolkit', toolkit_name: 'github', tool: 'list_issues' },
        { id: 'n2', type: 'agent', tool: 'Reviewer' },
        { id: 'n3', type: 'loop_from_tool', loop_toolkit_name: 'jira', loop_tool: 'search' },
        { id: 'n4', type: 'llm', tool_names: { github: ['create_pr'], slack: ['post_message'] } },
        { id: 'n5', type: 'code', tool: 'ignored_for_non_app_node' },
      ],
    });

    expect(getReferencedNames(refs, TOOL_REF_KINDS.toolkit)).toEqual(
      new Set(['github', 'jira', 'slack']),
    );
    expect(getReferencedNames(refs, TOOL_REF_KINDS.application)).toEqual(new Set(['Reviewer']));
    expect(refs.get('toolkit:github')?.nodeIds).toEqual(['n1', 'n4']);
    expect(refs.get('toolkit:github')?.toolNames).toEqual(new Set(['list_issues', 'create_pr']));
  });

  it('resolves attachment ref names, keys, display names, and lookups', () => {
    const tools: PickerToolEntry[] = [
      { id: 10, type: 'github', name: 'My GitHub', toolkit_name: 'gh_prod' },
      { type: 'application', name: 'Planner', settings: { application_id: 42 } },
    ];
    expect(getAttachmentRefName(tools[0])).toBe('gh_prod');
    expect(getAttachmentRefKey(tools[0])).toBe('toolkit:gh_prod');
    expect(getAttachmentRefKey(tools[1])).toBe('application:Planner');
    expect(getToolkitDisplayName(tools[0])).toBeTruthy();

    expect(findAttachedToolkit(tools, 'gh_prod')).toBe(tools[0]);
    expect(findAttachedToolkit(tools, 'My GitHub')).toBe(tools[0]);
    expect(findAttachedToolkit(tools, 'Planner')).toBeUndefined();
  });

  it('groups attachments in toolkits / MCPs / agents / pipelines order and respects filters', () => {
    const tools: PickerToolEntry[] = [
      { type: 'application', name: 'SubPipe', agent_type: 'pipeline', settings: { application_id: 4 } },
      { type: 'application', name: 'AgentA', agent_type: 'classic', settings: { application_id: 3 } },
      { id: 2, type: 'mcp', name: 'McpSrv', toolkit_name: 'mcp_srv', meta: { mcp: true } },
      { id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' },
      { id: 99, type: 'artifact', name: 'Attachments', toolkit_name: 'attachments' },
    ];
    const refs = collectToolReferences({
      nodes: [{ id: 'node-1', type: 'toolkit', toolkit_name: 'gh' }],
    });

    const groups = groupAttachmentsByKind({
      tools,
      refs,
      hiddenKinds: [PICKER_KINDS.mcp],
      hiddenToolIds: [99],
    });

    expect(groups.map((g) => g.kind)).toEqual([
      PICKER_KINDS.toolkit,
      PICKER_KINDS.agent,
      PICKER_KINDS.pipeline,
    ]);
    expect(groups[0]?.items[0]?.usedBy).toEqual(['node-1']);
    expect(groups[1]?.items[0]?.usedBy).toEqual([]);
  });
});
