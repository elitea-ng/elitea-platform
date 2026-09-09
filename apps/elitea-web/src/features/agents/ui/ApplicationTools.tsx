import type { ReactNode } from 'react';
import { useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { markAllDuplicatesByMultipleKeys } from '@/shared/lib/array';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { useSelectedProjectId } from '../api/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../api/useToolkitTypeSchemas';
import type { AvailableInternalTool } from '../lib/internalTools';
import { useAvailableInternalTools } from '../lib/internalTools';
import { genToolkitName } from '../lib/toolkitLabel';
import type { AgentToolAssociation } from '../lib/types';
import { useIsMcpVisible } from '../api/useIsMcpVisible';

import { AgentInternalToolSwitch } from './AgentInternalToolSwitch';
import { ToolMenu } from './ToolMenu';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/ApplicationTools.jsx`.
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): `tools`/`internalTools` are props,
 * `onInternalToolsChange` replaces `formik.setFieldValue(
 * 'version_details.meta.internal_tools', ...)`.
 *
 * `useGetCurrentToolkitSchemas`/`ToolkitsHelpers.genToolkitName` (baseline:
 * `features/toolkits/lib/{hooks,helpers}`) become this sub-unit's OWN local
 * `useToolkitTypeSchemas`/`genToolkitName` (`../api/useToolkitTypeSchemas.ts`,
 * `../lib/toolkitLabel.ts`) — unit A4 (toolkits) has not landed in this
 * worktree, and `no-sideways-features` forbids importing it either way; both
 * local files call the exact same real generated endpoint / reproduce the
 * exact same pure comparison, per their own doc comments.
 *
 * `ToolMenu` (sibling A1e's `./ToolMenu.tsx`, landed) is rendered directly —
 * its real prop surface (`applicationId`/`onToolsChanged`/`onAttachToolkit`/
 * `onAttachMcp`, all but `applicationId` optional) matches what this
 * component already has to offer.
 *
 * `ToolCard` (sibling A1h's `./ToolCard.tsx`, also landed, but with a real
 * prop surface far richer than the baseline's `{tool, index, applicationId,
 * disabled, isDuplicate, entityProjectId}`): its `context`/`disassociate`/
 * `variables`/`toolSelection`/`validation`/`delegatedAuth`/`versionSelector`
 * props are each their own DI object (mutation callbacks, validation state,
 * per-tool variable-editing state) that only a page-level composition
 * actually owns — `ApplicationTools` has no such state to synthesize
 * correctly. Rather than fabricate placeholder handler objects for a
 * contract this component cannot honestly satisfy, `ApplicationTools` keeps
 * exactly what IS its own baseline logic (fetching schemas, sorting,
 * duplicate-marking, MCP-visibility filtering — `markedDuplicateTools`
 * below) and delegates the actual per-tool render to an injected
 * `renderToolCard` callback, which the real page-level caller — who DOES
 * own the disassociate/variables/toolSelection state `ToolCard` needs —
 * supplies as `(tool, index, isDuplicate) => <ToolCard ... />`.
 * `AgentToolAssociation` (`../lib/types.ts`) is reused for the `tools` prop
 * so values handed to `renderToolCard` need no reshaping.
 *
 * `isMcpToolkit` (`entities/toolkit`, already promoted) is NOT reused here:
 * its parameter type is the full `Toolkit` catalogue record (`id`/`name`/
 * `type` all required); `AgentToolAssociation` rows have `type` OPTIONAL and
 * no catalogue `id`/`name` guarantee — the two types are not structurally
 * compatible. `isMcpAssociation` below reproduces the same three-way check
 * (`shared/lib/helpers/mcp.helpers.js:7-14`, baseline) directly against the
 * looser association shape instead.
 *
 * `AGENT_TOUR_TARGET_IDS` (`features/interactive-tours`) is dropped: that
 * domain does not exist in this worktree and is out of this Wave-2 batch's
 * scope entirely (agents/pipelines/toolkits only); `data-tour` attributes
 * are omitted rather than pointed at a feature that will never legally be
 * importable here (`no-sideways-features`, no carve-out).
 */
export interface ApplicationToolsProps {
  readonly tools: readonly AgentToolAssociation[];
  readonly internalTools: readonly string[];
  readonly onInternalToolsChange: (next: readonly string[]) => void;
  readonly applicationId?: number | string | undefined;
  /** Renders one `markedDuplicateTools` entry — see the module doc comment for why this is injected rather than a direct `ToolCard` render. */
  readonly renderToolCard: (tool: AgentToolAssociation, index: number, isDuplicate: boolean) => ReactNode;
  /**
   * Forwarded to `ToolMenu`, fired after any successful attach. Added when
   * this component was first actually mounted (#307): `ToolMenu` invalidates
   * only its OWN `getApplication` cache entry and documents, in its "known
   * integration gap" paragraph, that a parent owning a SEPARATE `tools` list
   * "must refetch its own copy on `onToolsChanged` too" — but this component
   * (that exact parent) rendered `<ToolMenu applicationId={…} />` with no
   * callback at all, so an attach could never reach the `tools` prop it
   * renders from. The tool appeared only after a manual page reload.
   */
  readonly onToolsChanged?: (() => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly title?: string | undefined;
  readonly hidePythonSandbox?: boolean | undefined;
  readonly isPipeline?: boolean | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

function isMcpAssociation(tool: AgentToolAssociation): boolean {
  if (tool.type === 'mcp' || tool.type?.startsWith('mcp_')) return true;
  return tool.meta?.mcp === true;
}

export function ApplicationTools({
  tools,
  internalTools,
  onInternalToolsChange,
  applicationId,
  renderToolCard,
  onToolsChanged,
  disabled,
  title = 'Tools',
  hidePythonSandbox = false,
  isPipeline = false,
  sx,
}: ApplicationToolsProps): ReactNode {
  const sortedToolsRef = useRef<readonly AvailableInternalTool[] | null>(null);
  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const isMcpVisible = useIsMcpVisible();
  const availableInternalTools = useAvailableInternalTools({ includeAgentOnly: true });
  const [showAllInternalTools, setShowAllInternalTools] = useState(false);

  const sortedInternalTools = useMemo(() => {
    if (sortedToolsRef.current && sortedToolsRef.current.length === availableInternalTools.length) {
      return sortedToolsRef.current;
    }
    const sorted = [...availableInternalTools].sort((a, b) => {
      const aEnabled = internalTools.includes(a.name);
      const bEnabled = internalTools.includes(b.name);
      if (aEnabled === bEnabled) return 0;
      return aEnabled ? -1 : 1;
    });
    sortedToolsRef.current = sorted;
    return sorted;
  }, [availableInternalTools, internalTools]);

  const { displayedInternalTools, canToggleTools } = useMemo(() => {
    const totalTools = sortedInternalTools.length;
    const selectedCount = internalTools.length;
    const minToolsToShow = Math.max(4, selectedCount);
    const canToggle = totalTools > minToolsToShow;
    if (showAllInternalTools || !canToggle) {
      return { displayedInternalTools: sortedInternalTools, canToggleTools: canToggle };
    }
    return { displayedInternalTools: sortedInternalTools.slice(0, minToolsToShow), canToggleTools: canToggle };
  }, [sortedInternalTools, showAllInternalTools, internalTools.length]);

  const pipelineVisibleTools = useMemo(
    () => (isPipeline ? sortedInternalTools.filter((t) => t.name === 'attachments') : displayedInternalTools),
    [isPipeline, sortedInternalTools, displayedInternalTools],
  );

  const shouldShowInternalTools = isPipeline
    ? pipelineVisibleTools.length > 0
    : !hidePythonSandbox && sortedInternalTools.length > 0;

  const markedDuplicateTools = useMemo(
    () =>
      markAllDuplicatesByMultipleKeys(
        tools
          .map((tool, originalIndex) => ({
            tool,
            originalIndex,
            type: tool.type,
            label: genToolkitName(tool, toolkitTypeSchemas),
          }))
          .filter(({ tool }) => isMcpVisible || !isMcpAssociation(tool)),
        ['type', 'label'],
      ),
    [toolkitTypeSchemas, tools, isMcpVisible],
  );

  const onToggleInternalTool = (name: string, checked: boolean): void => {
    onInternalToolsChange(checked ? [...internalTools, name] : internalTools.filter((t) => t !== name));
  };

  return (
    <BasicAccordion
      data-testid="agent-toolkits-section"
      showMode="left"
      slotSx={{ accordion: accordionSx, ...(sx !== undefined ? { root: sx } : {}) }}
      items={[
        {
          title,
          content: (
            <Box sx={containerSx}>
              {!disabled && (
                <Box sx={toolMenuWrapperSx}>
                  <ToolMenu
                    applicationId={applicationId}
                    onToolsChanged={onToolsChanged}
                  />
                </Box>
              )}

              {markedDuplicateTools.map(({ tool, originalIndex, isDuplicate }) => (
                <Box key={originalIndex}>{renderToolCard(tool, originalIndex, isDuplicate)}</Box>
              ))}

              {shouldShowInternalTools && (
                <Box sx={internalToolsContainerSx}>
                  <Typography
                    variant="subtitle"
                    sx={internalToolsTitleSx}
                  >
                    {t('features.agents.applicationTools.internalToolsTitle', 'INTERNAL TOOLS')}
                  </Typography>
                  <Box sx={internalToolsGridSx}>
                    {pipelineVisibleTools.map((tool) => (
                      <AgentInternalToolSwitch
                        key={tool.name}
                        name={tool.name}
                        title={tool.title}
                        icon={tool.icon}
                        checked={internalTools.includes(tool.name)}
                        onCheckedChange={(checked) => onToggleInternalTool(tool.name, checked)}
                        disabled={disabled || !tool.available}
                        infoTooltip={tool.infoTooltip}
                        unavailableReason={tool.unavailableReason}
                      />
                    ))}
                  </Box>
                  {!isPipeline && canToggleTools && (
                    <Box sx={showMoreContainerSx}>
                      <Typography
                        component="button"
                        variant="bodySmall"
                        onClick={() => setShowAllInternalTools(!showAllInternalTools)}
                        sx={showMoreButtonSx}
                      >
                        {showAllInternalTools
                          ? t('features.agents.applicationTools.showLess', 'Show less')
                          : t('features.agents.applicationTools.showAll', 'Show all')}
                      </Typography>
                    </Box>
                  )}
                </Box>
              )}
            </Box>
          ),
        },
      ]}
    />
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
});

const containerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: 1,
};

const toolMenuWrapperSx: SxProps<Theme> = {
  margin: '0.75rem 0',
};

const internalToolsTitleSx: SxProps<Theme> = {
  margin: '0.5rem 0 1rem',
};

const internalToolsContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
};

const internalToolsGridSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(max(15rem, calc(50% - 0.25rem)), 1fr))',
  gap: '0.5rem',
  width: '100%',
};

const showMoreContainerSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'flex-start',
  marginTop: '0.75rem',
};

const showMoreButtonSx: SxProps<Theme> = (theme: Theme) => ({
  border: 'none',
  background: 'none',
  color: theme.vars.palette.primary.main,
  padding: '0.375rem 0',
  cursor: 'pointer',
  '&:hover': {
    backgroundColor: 'transparent',
    opacity: 0.8,
  },
});
