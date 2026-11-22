import { describe, expect, it } from 'vitest';

import { reconcileAttachments } from './attachmentReconcile.helpers';
import type { PickerToolEntry } from './toolPicker.types';

describe('attachmentReconcile.helpers', () => {
  it('drops newly-added or previously-used attachments when no node references them', () => {
    const usedOnLoad: PickerToolEntry = {
      id: 1,
      type: 'github',
      toolkit_name: 'gh',
      settings: { selected_tools: ['list_issues'] },
    };
    const addedInSession: PickerToolEntry = {
      id: 2,
      type: 'jira',
      toolkit_name: 'jira',
      settings: { selected_tools: ['search'] },
    };
    const legacyUnused: PickerToolEntry = {
      id: 3,
      type: 'slack',
      toolkit_name: 'slack',
      settings: { selected_tools: ['post'] },
    };

    const next = reconcileAttachments({
      tools: [usedOnLoad, addedInSession, legacyUnused],
      initialTools: [usedOnLoad, legacyUnused],
      initialYamlJsonObject: { nodes: [{ id: 'n1', type: 'toolkit', toolkit_name: 'gh', tool: 'list_issues' }] },
      yamlJsonObject: { nodes: [] },
    });

    // Legacy unused is preserved; usedOnLoad & addedInSession are dropped
    expect(next).toEqual([legacyUnused]);
  });

  it('narrows selected_tools when node usage changes and stays idempotent', () => {
    const gh: PickerToolEntry = {
      id: 1,
      type: 'github',
      toolkit_name: 'gh',
      settings: { selected_tools: ['list_issues', 'create_pr'], available_tools: ['list_issues', 'create_pr'] },
    };
    const initialTools = [gh];
    const initialYaml = { nodes: [{ id: 'n1', type: 'toolkit', toolkit_name: 'gh', tool: 'list_issues' }] };
    const updatedYaml = { nodes: [{ id: 'n1', type: 'toolkit', toolkit_name: 'gh', tool: 'create_pr' }] };

    const narrowed = reconcileAttachments({
      tools: initialTools,
      initialTools,
      initialYamlJsonObject: initialYaml,
      yamlJsonObject: updatedYaml,
    });

    expect(narrowed[0]?.settings?.selected_tools).toEqual(['create_pr']);

    // Second pass returns the exact same array reference (idempotent)
    const secondPass = reconcileAttachments({
      tools: narrowed,
      initialTools,
      initialYamlJsonObject: initialYaml,
      yamlJsonObject: updatedYaml,
    });
    expect(secondPass).toBe(narrowed);
  });

  it('keeps initial selected_tools untouched when node usage matches load time', () => {
    const gh: PickerToolEntry = {
      id: 1,
      type: 'github',
      toolkit_name: 'gh',
      settings: { selected_tools: ['list_issues', 'extra_saved_tool'] },
    };
    const yaml = { nodes: [{ id: 'n1', type: 'toolkit', toolkit_name: 'gh', tool: 'list_issues' }] };

    const result = reconcileAttachments({
      tools: [gh],
      initialTools: [gh],
      initialYamlJsonObject: yaml,
      yamlJsonObject: yaml,
    });
    expect(result[0]?.settings?.selected_tools).toEqual(['list_issues', 'extra_saved_tool']);
  });
});
