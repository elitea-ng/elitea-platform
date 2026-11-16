import { useMemo } from 'react';

import { toolkitTools } from '@/entities/toolkit';

import type { AgentToolAssociation, AgentToolAvailableToolOption } from '../lib/types';

import { normalizeAvailableToolOptions } from './ToolCard.lib';

/**
 * The tools an OPEN toolkit card offers for this agent to choose from (#2146).
 *
 * Normally the toolkit's own enabled tools, which `toAgentToolAssociations`
 * puts in `settings.available_tools`. A toolkit that enables none explicitly
 * offers all of its tools, and that catalogue lives behind the
 * `toolkit_available_tools` route, so the caller mounts this hook only once the card is
 * expanded: a page of cards must not each start a discovery run on load.
 */
export function useToolOptions(tool: AgentToolAssociation, projectId: string | number | undefined): readonly AgentToolAvailableToolOption[] {
  const stored = useMemo(() => normalizeAvailableToolOptions(tool.settings?.available_tools ?? []), [tool.settings?.available_tools]);
  const isToolkit = tool.type !== 'application' && tool.tool_id !== undefined;
  const catalogue = toolkitTools.useToolkitTools({
    projectId: projectId === undefined ? undefined : String(projectId),
    toolkitId: tool.tool_id === undefined ? undefined : String(tool.tool_id),
    enabled: isToolkit && stored.length === 0,
  });
  const fromCatalogue = useMemo(() => normalizeAvailableToolOptions(catalogue.toolNames), [catalogue.toolNames]);
  return stored.length > 0 ? stored : fromCatalogue;
}
