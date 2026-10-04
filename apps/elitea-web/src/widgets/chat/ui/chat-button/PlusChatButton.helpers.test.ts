import { describe, expect, it, vi } from 'vitest';

import { resolveActiveSubmenuView } from './PlusChatButton.helpers';

const baseParams = () => ({
  participants: undefined,
  entities: undefined,
  availableTools: [],
  enabledToolNames: undefined,
  onSelect: vi.fn(),
  onToggle: vi.fn(),
  onCreate: { agents: vi.fn(), pipelines: vi.fn(), toolkits: vi.fn(), mcps: vi.fn() },
});

describe('resolveActiveSubmenuView', () => {
  it('returns empty items and no createConfig when submenu is null', () => {
    const result = resolveActiveSubmenuView(null, baseParams());
    expect(result.items).toEqual([]);
    expect(result.createConfig).toBeUndefined();
  });

  it('builds agent submenu items from participants', () => {
    const onSelect = vi.fn();
    const result = resolveActiveSubmenuView('agents', {
      ...baseParams(),
      participants: [{ id: 'a1', name: 'Bot' }, { id: 'a2', name: 'Helper' }],
      onSelect,
    });
    expect(result.items).toHaveLength(2);
    expect(result.items[0]!.label).toBe('Bot');
    expect(result.items[0]!.key).toBe('agent-a1');
    result.items[0]!.onClick?.();
    expect(onSelect).toHaveBeenCalledWith({ id: 'a1', name: 'Bot' });
  });

  it('prefers the available agent catalog over attached participants', () => {
    const result = resolveActiveSubmenuView('agents', {
      ...baseParams(),
      participants: [{ id: 'attached', name: 'Attached' }],
      entities: { agents: [{ id: 'available', name: 'Available' }] },
    });

    expect(result.items).toHaveLength(1);
    expect(result.items[0]!.label).toBe('Available');
  });

  it('builds pipeline submenu from entities.pipelines', () => {
    const result = resolveActiveSubmenuView('pipelines', {
      ...baseParams(),
      entities: { pipelines: [{ id: 'p1', name: 'MyPipeline' }] },
    });
    expect(result.items[0]!.label).toBe('MyPipeline');
    expect(result.items[0]!.key).toBe('pipeline-p1');
  });

  it('builds toolkit submenu from entities.toolkits', () => {
    const onToggle = vi.fn();
    const result = resolveActiveSubmenuView('toolkits', {
      ...baseParams(),
      entities: {
        toolkits: [{ id: 't1', name: 'ToolA' }],
        getParticipantMenuState: () => ({ checked: true, pending: true }),
      },
      onToggle,
    });
    expect(result.items[0]!.label).toBe('ToolA');
    expect(result.items[0]!.checked).toBe(true);
    expect(result.items[0]!.pending).toBe(true);
    result.items[0]!.onClick?.();
    expect(onToggle).toHaveBeenCalledWith({ id: 't1', name: 'ToolA' });
  });

  it('builds mcps submenu from entities.mcps', () => {
    const result = resolveActiveSubmenuView('mcps', {
      ...baseParams(),
      entities: { mcps: [{ id: 'm1', name: 'McpServer' }] },
    });
    expect(result.items[0]!.label).toBe('McpServer');
  });

  it('builds tools submenu with checked state', () => {
    const onConfig = vi.fn();
    const result = resolveActiveSubmenuView('tools', {
      ...baseParams(),
      availableTools: [{ name: 'search', title: 'Web Search' }, { name: 'code', title: 'Code Exec' }],
      enabledToolNames: ['search'],
      onInternalToolsConfigChange: onConfig,
    });
    expect(result.items).toHaveLength(2);
    expect(result.items[0]!.checked).toBe(true);
    expect(result.items[1]!.checked).toBe(false);
    result.items[1]!.onClick?.();
    expect(onConfig).toHaveBeenCalledWith({ key: 'code', value: true });
  });

  it('returns empty items for attachments submenu', () => {
    const result = resolveActiveSubmenuView('attachments', baseParams());
    expect(result.items).toEqual([]);
  });

  it('shows createConfig for creatable submenus', () => {
    for (const key of ['agents', 'pipelines', 'toolkits', 'mcps'] as const) {
      const result = resolveActiveSubmenuView(key, baseParams());
      expect(result.createConfig?.showCreateNew).toBe(true);
    }
  });

  it('does not show createConfig for non-creatable submenus', () => {
    expect(resolveActiveSubmenuView('tools', baseParams()).createConfig?.showCreateNew).toBe(false);
    expect(resolveActiveSubmenuView('attachments', baseParams()).createConfig?.showCreateNew).toBe(false);
  });

  it('uses fallback label when entity has no name', () => {
    const result = resolveActiveSubmenuView('agents', {
      ...baseParams(),
      participants: [{ id: 'x' }],
    });
    expect(result.items[0]!.label).toBe('Agent 1');
  });

  it('uses index as key fallback when entity has no id', () => {
    const result = resolveActiveSubmenuView('agents', {
      ...baseParams(),
      participants: [{ name: 'NoId' }],
    });
    expect(result.items[0]!.key).toBe('agent-0');
  });

  it('names the entity on the create row of each category (#6629)', () => {
    const labels = (['agents', 'pipelines', 'toolkits', 'mcps'] as const).map(
      (key) => resolveActiveSubmenuView(key, baseParams()).createConfig?.createLabel,
    );
    expect(labels).toEqual(['Create Agent', 'Create Pipeline', 'Create Toolkit', 'Create MCP']);
  });
});
