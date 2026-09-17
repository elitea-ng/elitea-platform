import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { resolveMcpExposureField, resolveToolGroups } from './ToolsSectionParts';
import type { EditToolDetail, EditToolField, ToolSchema } from './types';

function passParams(editField: EditToolField): { readonly editField: EditToolField } {
  return { editField };
}

/**
 * ELITEA-2687 / ELITEA-2693 — the MCP access control.
 *
 * The STORED field is the assertion that matters: the control changed shape
 * and place, and `meta.mcp_options.available_by_mcp` did not. A toolkit saved
 * before this change has to open with the toggle already on.
 */
describe('resolveMcpExposureField', () => {
  const detail = (meta?: Record<string, unknown>): EditToolDetail => ({ settings: {}, ...(meta === undefined ? {} : { meta }) });

  it('renders nothing when MCP exposure is not offered by this deployment', () => {
    const { container } = renderWithTheme(<>{resolveMcpExposureField(false, detail(), passParams(vi.fn()), false)}</>);
    expect(container).toBeEmptyDOMElement();
  });

  it('is a SWITCH with the new label and an explanation, off by default for a new toolkit', () => {
    const { getByRole, getByTestId } = renderWithTheme(<>{resolveMcpExposureField(true, detail(), passParams(vi.fn()), false)}</>);
    const toggle = getByRole('switch', { name: 'Enable MCP access for selected tools' });
    expect(toggle).not.toBeChecked();
    expect(getByTestId('toolkit-mcp-access-hint')).toBeInTheDocument();
  });

  it('opens ON for a toolkit that already had MCP exposure enabled — the setting is preserved, not migrated (ELITEA-2693)', () => {
    const { getByRole } = renderWithTheme(
      <>{resolveMcpExposureField(true, detail({ mcp_options: { available_by_mcp: true } }), passParams(vi.fn()), false)}</>,
    );
    expect(getByRole('switch', { name: 'Enable MCP access for selected tools' })).toBeChecked();
  });

  it('writes the SAME stored field the checkbox wrote', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const { getByRole } = renderWithTheme(<>{resolveMcpExposureField(true, detail(), passParams(editField), false)}</>);
    await user.click(getByRole('switch', { name: 'Enable MCP access for selected tools' }));
    expect(editField).toHaveBeenCalledWith('meta.mcp_options.available_by_mcp', true);
  });

  it('turns the stored flag back off', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const { getByRole } = renderWithTheme(
      <>{resolveMcpExposureField(true, detail({ mcp_options: { available_by_mcp: true } }), passParams(editField), false)}</>,
    );
    await user.click(getByRole('switch', { name: 'Enable MCP access for selected tools' }));
    expect(editField).toHaveBeenCalledWith('meta.mcp_options.available_by_mcp', false);
  });
});

describe('resolveToolGroups', () => {
  it('reads the served classification and its order off selected_tools', () => {
    const schema = {
      properties: {
        selected_tools: { tool_groups: { read_file: 'read' }, tool_group_order: ['read', 'delete'] },
      },
    } as unknown as ToolSchema;
    expect(resolveToolGroups(schema)).toEqual({ groups: { read_file: 'read' }, order: ['read', 'delete'] });
  });

  it('answers with NOTHING for a type the server did not classify — the flat-list fallback keys on exactly this (ELITEA-2688)', () => {
    const schema = { properties: { selected_tools: { items: { enum: ['a'] } } } } as unknown as ToolSchema;
    expect(resolveToolGroups(schema)).toEqual({});
    expect(resolveToolGroups({})).toEqual({});
  });

  it('treats an EMPTY served map as no classification rather than as "every tool is ungrouped"', () => {
    const schema = { properties: { selected_tools: { tool_groups: {} } } } as unknown as ToolSchema;
    expect(resolveToolGroups(schema)).toEqual({});
  });
});
