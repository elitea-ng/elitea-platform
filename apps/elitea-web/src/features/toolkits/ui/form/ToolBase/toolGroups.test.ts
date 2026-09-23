import { describe, expect, it } from 'vitest';

import { buildToolGroupSections, toggleGroupSelection, toolGroupDescriptor } from './toolGroups';
import type { ToolActionOption } from './ToolActionsItems';

const OPTIONS: readonly ToolActionOption[] = [
  { value: 'read_file', label: 'Read file' },
  { value: 'get_file_metadata', label: 'Get file metadata' },
  { value: 'list_files', label: 'List files' },
  { value: 'create_file', label: 'Create file' },
  { value: 'append_data', label: 'Append data' },
  { value: 'delete_file', label: 'Delete file' },
  { value: 'execute_generic_rq', label: 'Execute generic rq' },
];

const GROUPS: Record<string, string> = {
  read_file: 'read',
  get_file_metadata: 'read',
  list_files: 'read',
  create_file: 'create_update',
  append_data: 'create_update',
  delete_file: 'delete',
  execute_generic_rq: 'execute',
};

function build(overrides: Partial<Parameters<typeof buildToolGroupSections>[0]> = {}) {
  return buildToolGroupSections({
    toolsOptions: OPTIONS,
    toolGroups: GROUPS,
    groupOrder: ['read', 'create_update', 'delete', 'execute'],
    selectedTools: [],
    query: '',
    ...overrides,
  });
}

describe('toolGroupDescriptor', () => {
  it('names each group, its consequence badge and its explanation (ELITEA-2684)', () => {
    expect(toolGroupDescriptor('read')).toEqual({
      label: 'Read',
      badge: 'Read-only',
      hint: 'Returns data. Nothing is created, changed or destroyed.',
    });
    expect(toolGroupDescriptor('create_update').label).toBe('Create & update');
    expect(toolGroupDescriptor('create_update').badge).toBe('Changes data');
    expect(toolGroupDescriptor('delete').badge).toBe('Destructive');
    expect(toolGroupDescriptor('execute').badge).toBe('Unrestricted');
  });

  it('renders an unknown group id rather than dropping it — an undisplayed group hides every tool in it', () => {
    expect(toolGroupDescriptor('quarantine')).toEqual({ label: 'quarantine', badge: '', hint: '' });
  });
});

describe('buildToolGroupSections', () => {
  it('emits the groups in the served fixed order (ELITEA-2684)', () => {
    expect(build().sections.map((section) => section.id)).toEqual(['read', 'create_update', 'delete', 'execute']);
  });

  it('hides a group the toolkit has no tools for — the artifact toolkit has no Execute tools (ELITEA-2686)', () => {
    const sections = build({
      toolsOptions: OPTIONS.filter((option) => option.value !== 'execute_generic_rq'),
    }).sections;
    expect(sections.map((section) => section.id)).toEqual(['read', 'create_update', 'delete']);
  });

  it('sorts alphabetically WITHIN each group (ELITEA-2686)', () => {
    const read = build().sections[0];
    expect(read?.tools.map((tool) => tool.label)).toEqual(['Get file metadata', 'List files', 'Read file']);
  });

  it('counts selected out of total per group', () => {
    const sections = build({ selectedTools: ['read_file', 'list_files', 'delete_file'] }).sections;
    expect(sections[0]).toMatchObject({ id: 'read', selectedCount: 2, totalCount: 3, allSelected: false });
    expect(sections[2]).toMatchObject({ id: 'delete', selectedCount: 1, totalCount: 1, allSelected: true });
  });

  it('keeps the counts over the WHOLE group while a search filter is active (ELITEA-2690)', () => {
    const sections = build({ selectedTools: ['read_file', 'list_files'], query: 'metadata' }).sections;
    const read = sections.find((section) => section.id === 'read');
    // One Read tool matches "metadata", but the header still answers for all three.
    expect(read?.tools.map((tool) => tool.value)).toEqual(['get_file_metadata']);
    expect(read).toMatchObject({ selectedCount: 2, totalCount: 3 });
  });

  it('matches the display name AND the raw tool name, case-insensitively (ELITEA-2695)', () => {
    expect(build({ query: 'READ' }).sections.flatMap((s) => s.tools.map((t) => t.value))).toEqual(['read_file']);
    expect(build({ query: 'read_f' }).sections.flatMap((s) => s.tools.map((t) => t.value))).toEqual(['read_file']);
    expect(build({ query: 'Read file' }).sections.flatMap((s) => s.tools.map((t) => t.value))).toEqual(['read_file']);
  });

  it('hides a group whose tools are all filtered out (ELITEA-2695 step 8)', () => {
    expect(build({ query: 'delete' }).sections.map((s) => s.id)).toEqual(['delete']);
  });

  it('reports noMatches only when the toolkit HAS tools and none matches', () => {
    expect(build({ query: 'xyznonexistent123' })).toMatchObject({ sections: [], noMatches: true });
    expect(build({ query: 'file' }).noMatches).toBe(false);
    // An empty toolkit is not a failed search.
    expect(build({ toolsOptions: [], query: 'anything' }).noMatches).toBe(false);
  });

  it('carries every tool of the group in allToolValues, filter or no filter', () => {
    const read = build({ query: 'metadata' }).sections[0];
    expect(read?.tools).toHaveLength(1);
    expect(read?.allToolValues).toEqual(['read_file', 'get_file_metadata', 'list_files']);
  });

  it('puts an unclassified tool in the first group rather than dropping it', () => {
    const sections = build({
      toolsOptions: [...OPTIONS, { value: 'mystery_tool', label: 'Mystery tool' }],
    }).sections;
    const read = sections.find((section) => section.id === 'read');
    expect(read?.allToolValues).toContain('mystery_tool');
  });

  it('appends a served group this build has no order entry for, rather than dropping it', () => {
    const sections = buildToolGroupSections({
      toolsOptions: [{ value: 'quarantine_thing', label: 'Quarantine thing' }, ...OPTIONS],
      toolGroups: { ...GROUPS, quarantine_thing: 'quarantine' },
      groupOrder: ['read', 'create_update', 'delete', 'execute'],
      selectedTools: [],
      query: '',
    }).sections;
    expect(sections.map((section) => section.id)).toEqual(['read', 'create_update', 'delete', 'execute', 'quarantine']);
  });
});

describe('toggleGroupSelection', () => {
  it('selects every tool of the group without touching the others (ELITEA-2685)', () => {
    expect(
      toggleGroupSelection({
        selectedTools: ['delete_file', 'read_file'],
        groupTools: ['read_file', 'get_file_metadata', 'list_files'],
        select: true,
      }),
    ).toEqual(['delete_file', 'read_file', 'get_file_metadata', 'list_files']);
  });

  it('deselects every tool of the group without touching the others', () => {
    expect(
      toggleGroupSelection({
        selectedTools: ['delete_file', 'read_file', 'list_files'],
        groupTools: ['read_file', 'get_file_metadata', 'list_files'],
        select: false,
      }),
    ).toEqual(['delete_file']);
  });

  it('preserves the stored order and returns the same array when nothing changes', () => {
    const selected = ['read_file', 'list_files'];
    expect(toggleGroupSelection({ selectedTools: selected, groupTools: ['read_file'], select: true })).toBe(selected);
  });
});
