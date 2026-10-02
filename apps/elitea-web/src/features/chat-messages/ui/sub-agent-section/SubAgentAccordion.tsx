/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/sub-agent-section/
 * SubAgentAccordion.jsx` — renders a sub-agent execution accordion with
 * grouped tool actions and live status indicator.
 *
 * Uses `partitionActionsIntoBlocks` from `../../lib/subAgentGrouping`
 * for grouping actions by sub-agent invocation.
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/sub-agent-section/
 * SubAgentAccordion.jsx`.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { convertJsonToString } from '@/shared/lib/json';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import type { PartitionedBlock } from '../../lib/subAgentGrouping';

import type { SubAgentTool } from './subAgentIcon.helpers';
import { resolveSubAgentIcon } from './subAgentIcon.helpers';

/** @public Props for `SubAgentAccordion`. */
export interface SubAgentAccordionProps {
  /** The partitioned sub-agent blocks to render. */
  readonly blocks: readonly PartitionedBlock[];
  /** Whether the accordion is expanded by default. */
  readonly defaultExpanded?: boolean;
  /** Called when a tool action is clicked. */
  readonly onActionClick?: ((action: unknown) => void) | undefined;
  /** The invoking participant's tool list — resolves each block's header icon (baseline: `SubAgentAccordion.jsx:55`'s `resolveSubAgentIcon(name, tools, theme, agentType)`). */
  readonly tools?: readonly SubAgentTool[];
}

/** Renders a block's header: its resolved icon (if any) followed by its display name. */
function BlockTitle({ block, tools, theme }: { readonly block: PartitionedBlock; readonly tools: readonly SubAgentTool[] | undefined; readonly theme: Theme }): ReactNode {
  const name = block.kind === 'sub' ? block.name : '';
  const icon = resolveSubAgentIcon(name, tools, theme);
  const Icon = icon?.component;
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      {Icon && <Box sx={icon.sx}><Icon /></Box>}
      {name || 'Sub-agent'}
    </Box>
  );
}

/**
 * `SubAgentAccordion` — renders sub-agent execution as expandable accordions.
 * Each sub-agent invocation gets its own accordion with grouped tool actions.
 */
export function SubAgentAccordion({
  blocks,
  defaultExpanded = false,
  onActionClick,
  tools,
}: SubAgentAccordionProps): ReactNode {
  const theme = useTheme();

  if (!blocks?.length) return null;

  return (
    <Box sx={{ mt: 1 }}>
      {blocks.map((block, index) => {
        if (block.kind === 'coord') {
          return null; // Coordinator actions rendered inline
        }

        const isExpanded = defaultExpanded || block.pausedForResume;

        return (
          <BasicAccordion
            key={`${block.instanceKey}-${index}`}
            items={[
              {
                title: <BlockTitle block={block} tools={tools} theme={theme} />,
                content: (
                  <Box sx={{ px: 2, pb: 1 }}>
                    {block.actions.map((action, actionIndex) => (
                      <Box
                        key={`${String((action as unknown as Record<string, unknown>).id)}-${actionIndex}`}
                        component="pre"
                        onClick={() => onActionClick?.(action)}
                        sx={{
                          fontFamily: 'monospace',
                          // eslint-disable-next-line elitea/ad-hoc-font-size — inline code size
                          fontSize: '0.8rem',
                          p: 1,
                          mb: 0.5,
                          backgroundColor: 'action.hover',
                          // eslint-disable-next-line elitea/ad-hoc-radius — inline code border radius
                          borderRadius: 0.5,
                          cursor: 'pointer',
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                        }}
                      >
                        <Typography
                          variant="caption"
                          sx={{
                            display: 'block',
                            color: 'text.secondary',
                            mb: 0.5,
                          }}
                        >
                          {(action.name as string) || action.type || 'Action'}
                        </Typography>
                        {convertJsonToString(action.toolOutputs ?? '')}
                      </Box>
                    ))}
                    {block.pausedForResume && (
                      <Typography
                        variant="caption"
                        sx={{ color: 'warning.dark', fontStyle: 'italic' }}
                      >
                        Paused — awaiting resume
                      </Typography>
                    )}
                  </Box>
                ),
              },
            ]}
            defaultExpanded={isExpanded}
          />
        );
      })}
    </Box>
  );
}
