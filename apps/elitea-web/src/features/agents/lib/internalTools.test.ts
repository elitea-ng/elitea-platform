import { describe, expect, it, vi } from 'vitest';

import * as isMcpVisibleModule from '../api/useIsMcpVisible';
import * as selectedProjectIdModule from '../api/useSelectedProjectId';
import * as toolkitTypeSchemasModule from '../api/useToolkitTypeSchemas';
import * as runtimeCapabilitiesModule from '@/shared/api/runtimeCapabilities';

import { INTERNAL_TOOLS_LIST, INTERNAL_TOOL_ICONS, useAvailableInternalTools } from './internalTools';
import { renderHookWithProviders } from '../__tests__/testUtils';

describe('INTERNAL_TOOLS_LIST / INTERNAL_TOOL_ICONS', () => {
  it('every entry has a resolvable icon component', () => {
    for (const tool of INTERNAL_TOOLS_LIST) {
      expect(INTERNAL_TOOL_ICONS[tool.icon]).toBeDefined();
    }
  });

  it('includes the attachments tool, flagged agent-only', () => {
    const attachments = INTERNAL_TOOLS_LIST.find((tool) => tool.name === 'attachments');
    expect(attachments?.agentOnly).toBe(true);
  });

  it('includes image_generation with no toolkit gate (#1969)', () => {
    const imageGeneration = INTERNAL_TOOLS_LIST.find((tool) => tool.name === 'image_generation');
    expect(imageGeneration).toBeDefined();
    expect(imageGeneration).not.toHaveProperty('requiredToolkitType');
  });

  it('ELITEA-2783: lists Skills Builder and Project Context Builder with their own tooltips', () => {
    const skills = INTERNAL_TOOLS_LIST.find((tool) => tool.name === 'skills_builder');
    const projectContext = INTERNAL_TOOLS_LIST.find((tool) => tool.name === 'project_context_builder');

    expect(skills?.title).toBe('Skills Builder');
    expect(skills?.infoTooltip.text).toBe('Create and update Skills directly from chat.');
    expect(projectContext?.title).toBe('Project Context Builder');
    expect(projectContext?.infoTooltip.text).toBe('Create and update Project Context directly from chat.');

    // Neither is agent-only and neither is gated on a toolkit type: both are
    // offered in a plain conversation, which is the only place the cases
    // exercise them. A `requiredToolkitType` here would make the toggle
    // vanish in every project with no such provider.
    expect(skills?.agentOnly).toBeUndefined();
    expect(skills?.requiredToolkitType).toBeUndefined();
    expect(projectContext?.agentOnly).toBeUndefined();
    expect(projectContext?.requiredToolkitType).toBeUndefined();

    // The two are SEPARATE entries. A single combined toggle is the failure
    // ELITEA-2783 step 3/4 is written to catch.
    expect(skills).not.toBe(projectContext);
  });
});

describe('useAvailableInternalTools', () => {
  it('excludes agent-only tools when includeAgentOnly is not set', () => {
    vi.spyOn(selectedProjectIdModule, 'useSelectedProjectId').mockReturnValue('proj-1');
    vi.spyOn(toolkitTypeSchemasModule, 'useToolkitTypeSchemas').mockReturnValue({
      toolkitTypeSchemas: {},
      isFetching: false,
      isError: false,
      error: undefined,
    });
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    expect(result.current.some((tool) => tool.name === 'attachments')).toBe(false);
    vi.restoreAllMocks();
  });

  it('includes agent-only tools when includeAgentOnly is set', () => {
    vi.spyOn(selectedProjectIdModule, 'useSelectedProjectId').mockReturnValue('proj-1');
    vi.spyOn(toolkitTypeSchemasModule, 'useToolkitTypeSchemas').mockReturnValue({
      toolkitTypeSchemas: {},
      isFetching: false,
      isError: false,
      error: undefined,
    });
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);

    const { result } = renderHookWithProviders(() => useAvailableInternalTools({ includeAgentOnly: true }));
    expect(result.current.some((tool) => tool.name === 'attachments')).toBe(true);
    vi.restoreAllMocks();
  });

  it('excludes internal_mcp when MCP is not visible', () => {
    vi.spyOn(selectedProjectIdModule, 'useSelectedProjectId').mockReturnValue('proj-1');
    vi.spyOn(toolkitTypeSchemasModule, 'useToolkitTypeSchemas').mockReturnValue({
      toolkitTypeSchemas: {},
      isFetching: false,
      isError: false,
      error: undefined,
    });
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(false);

    const { result } = renderHookWithProviders(() => useAvailableInternalTools({ includeAgentOnly: true }));
    expect(result.current.some((tool) => tool.name === 'internal_mcp')).toBe(false);
    vi.restoreAllMocks();
  });

  it('shows image_generation enabled when the deployment reports an image model (#1969)', () => {
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);
    vi.spyOn(runtimeCapabilitiesModule, 'useRuntimeCapabilities').mockReturnValue({
      worker: 'rust',
      internal_tools: { image_generation: true },
      hidden_toolkit_types: [],
    });

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    const tool = result.current.find((entry) => entry.name === 'image_generation');
    expect(tool?.available).toBe(true);
    expect(tool?.unavailableReason).toBeUndefined();
    vi.restoreAllMocks();
  });

  it('shows image_generation disabled, with the reason, when no image model is configured (#1969)', () => {
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);
    vi.spyOn(runtimeCapabilitiesModule, 'useRuntimeCapabilities').mockReturnValue({
      worker: 'rust',
      internal_tools: { image_generation: false },
      hidden_toolkit_types: [],
    });

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    const tool = result.current.find((entry) => entry.name === 'image_generation');
    expect(tool).toBeDefined();
    expect(tool?.available).toBe(false);
    expect(tool?.unavailableReason).toBe('No image model is configured for this deployment');
    vi.restoreAllMocks();
  });

  it('shows image_generation disabled while the capabilities query is pending or failed (#1969)', () => {
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);
    vi.spyOn(runtimeCapabilitiesModule, 'useRuntimeCapabilities').mockReturnValue(undefined);

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    const tool = result.current.find((entry) => entry.name === 'image_generation');
    expect(tool?.available).toBe(false);
    expect(tool?.unavailableReason).toBe('Image generation availability could not be checked');
    // Every other tool keeps the "never disable on missing data" rule.
    expect(result.current.find((entry) => entry.name === 'planner')?.available).toBe(true);
    vi.restoreAllMocks();
  });

  it('shows image_generation disabled when the deployment does not name it (#1969)', () => {
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);
    vi.spyOn(runtimeCapabilitiesModule, 'useRuntimeCapabilities').mockReturnValue({
      worker: 'rust',
      internal_tools: {},
      hidden_toolkit_types: [],
    });

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    expect(result.current.find((entry) => entry.name === 'image_generation')?.available).toBe(false);
    vi.restoreAllMocks();
  });

  it('keeps the worker reason for the other tools the worker skips', () => {
    vi.spyOn(isMcpVisibleModule, 'useIsMcpVisible').mockReturnValue(true);
    vi.spyOn(runtimeCapabilitiesModule, 'useRuntimeCapabilities').mockReturnValue({
      worker: 'rust',
      internal_tools: { swarm: false },
      hidden_toolkit_types: [],
    });

    const { result } = renderHookWithProviders(() => useAvailableInternalTools());
    expect(result.current.find((entry) => entry.name === 'swarm')?.unavailableReason).toBe(
      'Not available on the Rust worker',
    );
    vi.restoreAllMocks();
  });
});
