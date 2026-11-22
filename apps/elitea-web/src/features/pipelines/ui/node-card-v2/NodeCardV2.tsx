import type { ReactNode } from 'react';
import { memo, useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { getNodeColor, isDeprecatedNodeType } from '../../lib/flow-editor/helpers/node.helpers';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import {
  getNodeCardV2Status,
  getNodeCardV2Summary,
  isMiniCardZoom,
  NODE_CARD_V2_WIDTH_REM,
  type NodeCardV2Status,
  type NodeCardV2SummaryData,
} from './nodeCardV2.helpers';
import { NodeCardV2Header } from './NodeCardV2Header';
import { NodeCardV2Summary } from './NodeCardV2Summary';

export interface NodeCardViewProps {
  readonly id: string;
  readonly name: string;
  readonly type: string;
  readonly isEntrypoint?: boolean | undefined;
  readonly selected?: boolean | undefined;
  readonly isPerforming?: boolean | undefined;
  readonly hasRunError?: boolean | undefined;
  readonly isRunningPipeline?: boolean | undefined;
  /** Optional current canvas zoom level; values `< 0.6` automatically enable mini-card mode. */
  readonly zoom?: number | undefined;
  /** Explicit mini-card override (takes precedence over `zoom` when provided). */
  readonly isMiniCard?: boolean | undefined;
  /** Optional YAML node used to derive default summary lines when `summary` is not provided. */
  readonly yamlNode?: YamlPipelineNode | undefined;
  readonly requiredInputs?: readonly string[] | undefined;
  /** Explicit summary override; falls back to `getNodeCardV2Summary(yamlNode, ...)`. */
  readonly summary?: NodeCardV2SummaryData | undefined;
  /** Config or admission issue messages displayed in the status tooltip. */
  readonly issues?: readonly string[] | undefined;
  /** Optional admission badge/icon slot compatible with `<NodeAdmissionIssues nodeId={id} />`. */
  readonly admissionBadge?: ReactNode;
  /** Normal source/target handle render slot, receiving whether the card is expanded. */
  readonly handles?: ((isExpanded: boolean) => ReactNode) | undefined;
  /** Dedicated `error_output` source handle slot compatible with `<NodeErrorOutputHandle ... />`. */
  readonly errorOutputHandle?: ReactNode;
  readonly onClick?: (() => void) | undefined;
}

function normalizeTestIdSegment(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'node';
}

function resolveBorderColor(theme: Theme, status: NodeCardV2Status, selected: boolean, isRunningPipeline: boolean): string {
  if (status === 'running') return theme.vars.palette.primary.main;
  if (status === 'error') return theme.vars.palette.error.main;
  if (selected && !isRunningPipeline) return theme.vars.palette.primary.main;
  if (status === 'needsConfiguration') return theme.vars.palette.warning.main;
  return theme.vars.palette.border.flowNode;
}

function containerSx(
  isExpanded: boolean,
  status: NodeCardV2Status,
  selected: boolean,
  isRunningPipeline: boolean,
): SxProps<Theme> {
  return (theme) => {
    const borderColor = resolveBorderColor(theme, status, selected, isRunningPipeline);
    const hasHover = !status && !(selected && !isRunningPipeline);
    return {
      display: 'flex',
      flexDirection: 'column',
      justifyContent: 'flex-start',
      alignItems: 'flex-start',
      width: NODE_CARD_V2_WIDTH_REM,
      height: 'auto',
      padding: isExpanded ? '0 0 1.25rem' : 0,
      boxSizing: 'border-box',
      borderRadius: theme.vars.shape.radiusMd,
      position: 'relative',
      border: `0.0625rem solid ${borderColor}`,
      backgroundColor: theme.vars.palette.background.tabPanel,
      transition: 'border-color 0.15s ease',
      cursor: 'pointer',
      ...(hasHover ? { '&:hover': { borderColor: theme.vars.palette.primary.main } } : {}),
    };
  };
}

function headerSx(isExpanded: boolean, type: string): SxProps<Theme> {
  return (theme) => ({
    width: '100%',
    display: 'flex',
    alignItems: 'center',
    padding: '0.75rem 1rem',
    boxSizing: 'border-box',
    overflow: 'hidden',
    backgroundColor: getNodeColor(type, theme),
    borderBottom: isExpanded ? `0.0625rem solid ${theme.vars.palette.border.flowNode}` : 'none',
    borderRadius: isExpanded
      ? `${theme.vars.shape.radiusMd} ${theme.vars.shape.radiusMd} 0 0`
      : theme.vars.shape.radiusMd,
  });
}

function resolveIsMiniCard(isMiniCardProp: boolean | undefined, zoom: number | undefined): boolean {
  if (isMiniCardProp !== undefined) return isMiniCardProp;
  return zoom !== undefined ? isMiniCardZoom(zoom) : false;
}

/**
 * Compact view-only Wave U2 node card (`NodeCardView`).
 *
 * Renders the header, 2-line read-only summary, `< 0.6` zoom mini-card mode,
 * admission/status icon slot, normal handle slot, and `error_output` source handle slot
 * without modifying the currently mounted `NodeCard`.
 */
export const NodeCardView = memo(function NodeCardView(props: NodeCardViewProps): ReactNode {
  const { id, name, type, yamlNode, requiredInputs, summary: summaryProp, admissionBadge, handles, errorOutputHandle, onClick } = props;
  const issues = props.issues ?? [];
  const isMiniCard = resolveIsMiniCard(props.isMiniCard, props.zoom);
  const isExpanded = !isMiniCard;
  const isDeprecated = isDeprecatedNodeType(type);

  const summary = useMemo(
    () => summaryProp ?? getNodeCardV2Summary(yamlNode, { type, isDeprecated, requiredInputs }),
    [isDeprecated, requiredInputs, summaryProp, type, yamlNode],
  );

  const status = getNodeCardV2Status({
    isPerforming: props.isPerforming,
    hasRunError: props.hasRunError,
    hasConfigIssues: issues.length > 0,
  });

  return (
    <Box
      sx={containerSx(isExpanded, status, Boolean(props.selected), Boolean(props.isRunningPipeline))}
      onClick={onClick}
      data-testid={`pipeline-node-v2-card-${normalizeTestIdSegment(id)}`}
      data-status={status ?? undefined}
      data-mini={isMiniCard ? 'true' : undefined}
      data-performing={props.isPerforming ? 'true' : 'false'}
    >
      <Box sx={headerSx(isExpanded, type)}>
        <NodeCardV2Header
          name={name}
          type={type}
          isEntrypoint={props.isEntrypoint}
          isDeprecated={isDeprecated}
          status={status}
          issues={issues}
          admissionBadge={admissionBadge}
        />
      </Box>
      {isExpanded ? (
        <NodeCardV2Summary
          primary={summary.primary}
          secondary={summary.secondary}
          isNotConfigured={issues.length > 0}
        />
      ) : null}
      {handles?.(isExpanded)}
      {errorOutputHandle}
    </Box>
  );
});
