/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/DecisionNode/DecisionNodeShared.jsx` (129 lines) — unit A2f.
 *
 * `DecisionOutputHelpers.getBorderColorAndTooltip` -> `getBorderColorAndTooltip`
 * from `../../../lib/flow-editor/helpers/decisionOutput.helpers.ts`
 * (already-landed A2c). `useEdges`/`useNodes` come straight from
 * `@xyflow/react` exactly like the baseline.
 *
 * DEVIATIONS FROM BASELINE, all forced by real, verified constraints:
 *
 *  1. `StyledChip` (baseline: `@/components/DataDisplay/StyledChip`) has no
 *     ported equivalent anywhere in `shared/ui/` (verified: no `StyledChip`
 *     export exists in this app). MUI's own `Chip` reproduces the same
 *     bordered/deletable pill with the baseline's own inline `sx`
 *     (`styledChip` below) applied directly — no new abstraction invented.
 *  2. `StyledTooltip` (baseline: `@/ComponentsLib/Tooltip`) -> MUI's
 *     `Tooltip` directly, matching the already-landed sibling convention
 *     (`ui/nodes/BaseNode/NodeCardHeader.tsx` imports `Tooltip` from
 *     `@mui/material/Tooltip` for the exact same "deprecated node" tooltip
 *     use case).
 *  3. The baseline colours `Chip`'s delete icon via a
 *     `'& .MuiChip-deleteIcon'` selector (`decisionOutputsStyles.styledChip`).
 *     R-T6 (`no-mui-internal-selector`) bans `.Mui<Component>-<slot>`
 *     selectors outside `shared/brand/mui-overrides/` — same rule
 *     `features/agents/ui/EnhancedCardToolActions.tsx`'s own doc comment
 *     already documents dropping an equivalent `& .MuiChip-icon` override
 *     for. Here the override is achievable without a selector at all:
 *     `RemoveIcon`'s SVG source (`shared/ui/icons/svg/remove-icon.svg`) is
 *     `fill="currentColor"` throughout, so wrapping it in a plain element
 *     with `color: theme.vars.palette.icon.fill.secondary` reproduces the
 *     exact same rendered colour (and its `:hover` — MUI's `Chip` already
 *     applies `.Mui-disabled`/hover states to the whole `deleteIcon` slot
 *     it's given) with zero selector.
 *  4. `borderRadius: '1.25rem'` (baseline's fully-rounded pill radius,
 *     ~half the chip's own 1.5rem height) has no exact match among this
 *     app's three radius tokens (R-T10: `radiusSm`/`radiusMd`/`radiusLg` =
 *     4/8/16px, `shared/brand/tokens/default.pack.json`). `radiusLg` (16px)
 *     is the closest available and is used here — a disclosed, minor visual
 *     approximation (very-rounded vs. fully pill-shaped at this chip's
 *     small size), not a functional change. `outputsBorderContainer`'s
 *     `.5rem` (8px) has an exact match (`radiusMd`) and needs no such
 *     approximation.
 *
 * The `i18next/no-literal-string` gate (R-T3) requires the one user-visible
 * string this file renders (`"Decision outputs"`) to go through `t()`
 * instead of the baseline's bare JSX literal.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import { useEdges, useNodes } from '@xyflow/react';

import { PipelineNodeTypes } from '../../../lib/flow-editor/constants/flowEditor.constants';
import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { getBorderColorAndTooltip } from '../../../lib/flow-editor/helpers/decisionOutput.helpers';
import type { DecisionOutputBorderColor } from '../../../lib/flow-editor/helpers/decisionOutput.helpers';
import type { FlowGraphEdge, FlowGraphNode } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import { HeadingChip } from '@/shared/ui/HeadingChip';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';
import { t } from '@/shared/i18n';

export interface DecisionOutputsProps {
  readonly id: string;
  readonly decisionOutput: readonly string[];
  readonly onRemoveOutput: (output: string) => () => void;
  readonly isRunningPipeline?: boolean | undefined;
  readonly disabled?: boolean | undefined;
}

export function DecisionOutputs(props: DecisionOutputsProps): ReactNode {
  const { id, decisionOutput, onRemoveOutput, isRunningPipeline, disabled } = props;
  const edges = useEdges() as unknown as readonly FlowGraphEdge[];
  const nodes = useNodes() as unknown as readonly FlowGraphNode[];
  const theme = useTheme();
  const styles = decisionOutputsStyles();

  const decisionOutputsInfo = getNodeFieldInfo(PipelineNodeTypes.Decision, NODE_FIELD_KEYS.decisionOutputs);

  return (
    <Box sx={styles.decisionOutputsContainer}>
      <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: '0.25rem' }}>
        <HeadingChip label={t('pipelines.flowEditor.decisionNode.decisionOutputs', 'Decision outputs')} />
        {decisionOutputsInfo ? <InfoTooltip title={decisionOutputsInfo} placement="top" /> : null}
      </Box>

      <Box sx={styles.outputsBorderContainer}>
        {decisionOutput.map(item => {
          const { borderColor, tooltip } = getBorderColorAndTooltip(edges, nodes, id, item);
          return (
            <Tooltip
              key={item}
              title={tooltip}
              placement="top"
            >
              <Chip
                label={item}
                disabled={isRunningPipeline || disabled}
                className="nopan nodrag nowheel"
                sx={styles.styledChip(borderColor)}
                deleteIcon={
                  <Box
                    component="span"
                    data-testid="decision-output-remove"
                    sx={{ display: 'inline-flex', color: theme.vars.palette.icon.fill.secondary }}
                  >
                    <RemoveIcon />
                  </Box>
                }
                onDelete={onRemoveOutput(item)}
              />
            </Tooltip>
          );
        })}
      </Box>
    </Box>
  );
}

export interface DecisionNodeCommonStyles {
  readonly inputEnhancerContainer: SxProps<Theme> & { readonly className: string };
}

/**
 * DISCLOSED TRIM: the baseline (`DecisionNodeShared.jsx:56-88`) also
 * returned `renderValueBox`/`removeIcon`/`multipleSelect`/`labelSX`/
 * `selectSX`/`valueItemSX` — internal `sx` slots consumed exclusively by the
 * baseline's own `Select.SingleSelect`'s custom `multiple` renderer
 * (`renderValue` prop, removable-chip-in-select UI). That component has no
 * ported equivalent: this app's promoted `shared/ui/SingleSelect` is
 * explicitly single-value-only (that file's own doc comment: "keeps the
 * single-value case only... The 12-prop budget... would also have forced
 * grouping most of the dropped surface"). `./LegacyDecisionNode.tsx`
 * rebuilds the "Decision input" field as a locally-composed multi-value
 * picker (see that file's own doc comment) that does not consume these
 * slots at all, so they are dead weight in this port, not a functional
 * loss — only `inputEnhancerContainer` (the `AIAssistantInput` wrapper,
 * still real UI in both node variants) is kept.
 */
export function commonComponentStyles(): DecisionNodeCommonStyles {
  return {
    inputEnhancerContainer: {
      marginBottom: '0rem',
      className: 'nopan nodrag nowheel',
    },
  };
}

interface DecisionOutputsStyles {
  readonly decisionOutputsContainer: SxProps<Theme>;
  readonly outputsBorderContainer: SxProps<Theme>;
  readonly styledChip: (borderStatus: DecisionOutputBorderColor) => SxProps<Theme>;
}

/**
 * `theme.vars.palette.status[borderStatus]` (a computed index) is invisible
 * to the §4.6 check 7 reference scan (`shared/brand/__tests__/
 * reference-scan.ts`'s `classifyMember`/`staticPath`: "theme.vars.palette
 * must be read through a static dotted path (no computed access, no
 * aliasing)") — resolved here via a static per-branch switch instead so
 * every token read is a literal dotted path the scan can see.
 */
function resolveBorderStatusColor(theme: Theme, borderStatus: DecisionOutputBorderColor): string {
  switch (borderStatus) {
    case 'rejected':
      return theme.vars.palette.status.rejected;
    case 'published':
      return theme.vars.palette.status.published;
    case 'onModeration':
      return theme.vars.palette.status.onModeration;
  }
}

function decisionOutputsStyles(): DecisionOutputsStyles {
  return {
    decisionOutputsContainer: {
      display: 'flex',
      flexDirection: 'column',
      width: '100%',
      padding: '.5rem 0rem',
      gap: '.5rem',
      overflow: 'hidden',
    },
    outputsBorderContainer: (theme: Theme) => ({
      width: '100%',
      borderRadius: theme.vars.shape.radiusMd,
      border: `.0625rem solid ${theme.vars.palette.border.lines}`,
      display: 'flex',
      alignItems: 'center',
      padding: '.75rem 1rem',
      gap: '.5rem',
      boxSizing: 'border-box',
      flexWrap: 'wrap',
    }),
    // `!important` (baseline: both the delete-icon colour and the border)
    // is dropped — R-T5 (`no-important-sx`) bans it outright with no
    // per-file waiver available here. Both declarations below already win
    // on ordinary specificity against MUI's own `Chip` defaults (an inline
    // `sx`-generated class always outranks the base `MuiChip-root` rule at
    // equal specificity, per MUI's own emotion-insertion order), so the
    // rendered result is unaffected.
    styledChip:
      borderStatus =>
      (theme: Theme) => ({
        padding: '0rem .5rem',
        height: '1.5rem',
        borderRadius: theme.vars.shape.radiusLg,
        marginBottom: '0rem',
        marginRight: '0rem',
        gap: '.25rem',
        border: `.0625rem solid ${resolveBorderStatusColor(theme, borderStatus)}`,
      }),
  };
}
