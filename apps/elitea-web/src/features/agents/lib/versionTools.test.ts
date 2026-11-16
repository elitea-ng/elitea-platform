import { describe, expect, it } from 'vitest';

import { toAgentToolAssociations } from './versionTools';

describe('toAgentToolAssociations', () => {
  it('reads an entity_tool_mapping row\'s settings blob from `config` (where fetchVersionDetails puts it), not `settings`', () => {
    const [tool] = toAgentToolAssociations([
      { id: 5, tool_id: 77, entity_type: 'agent', name: 'Github', type: 'github', config: { url: 'https://github.example' } },
    ]);
    // Everything under ToolCard reads `tool.settings` only — a row left with
    // its blob under `config` renders with no URL and an open-in-new-tab
    // button that goes nowhere.
    expect(tool?.settings?.url).toBe('https://github.example');
    expect(tool?.tool_id).toBe(77);
  });

  it('falls back to `settings` for legacy application_tools rows', () => {
    const [tool] = toAgentToolAssociations([
      { id: 9, name: 'SubAgent', type: 'application', settings: { application_id: 3, application_version_id: 4 } },
    ]);
    expect(tool?.settings?.application_id).toBe(3);
    expect(tool?.settings?.application_version_id).toBe(4);
  });

  it('folds the top-level `selected_tools` into settings, where ToolCard reads it from', () => {
    const [tool] = toAgentToolAssociations([
      { id: 5, tool_id: 77, type: 'github', selected_tools: ['create_issue', 'list_issues'], config: {} },
    ]);
    expect(tool?.settings?.selected_tools).toEqual(['create_issue', 'list_issues']);
  });

  it('offers the toolkit\'s own enabled tools as the choices and keeps the attachment\'s subset as the selection (#2146)', () => {
    const [tool] = toAgentToolAssociations([
      { id: 5, tool_id: 77, type: 'github', selected_tools: ['list_issues'], config: { selected_tools: ['create_issue', 'list_issues'] } },
    ]);
    expect(tool?.settings?.available_tools).toEqual(['create_issue', 'list_issues']);
    expect(tool?.settings?.selected_tools).toEqual(['list_issues']);
  });

  it('tolerates a null/absent blob and a non-array selected_tools without throwing', () => {
    expect(toAgentToolAssociations([{ id: 1 }, { id: 2, config: null, selected_tools: {} }])).toEqual([
      { id: 1, settings: {} },
      { id: 2, settings: {} },
    ]);
    expect(toAgentToolAssociations(undefined)).toEqual([]);
  });
});
