/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/CustomHandle.jsx` (225 lines, unit A2e).
 *
 * DISCLOSED DEVIATIONS, each forced by a real, verified constraint:
 *
 *  - `useTheme` (baseline: `@mui/material`, which itself just re-exports
 *    `@mui/material/styles`'s) -> imported directly from
 *    `@mui/material/styles`, matching this app's `no-restricted-imports`
 *    gate (R-T4) and every other ported file's convention.
 *  - `theme.palette.*` -> `theme.vars.palette.*` (R-T7).
 *  - `borderRadius: '.375rem'` (a 6px collapsed-handle corner) has no exact
 *    token match (`radiusSm`=4px, `radiusMd`=8px, `radiusLg`=16px --
 *    `shared/brand/tokens/default.pack.json`) -- `radiusSm` used as the
 *    closest smaller-corner token (R-T10 bans the literal outright either
 *    way). The expanded-state `'1.25rem'` (a full stadium/pill shape at
 *    this handle's ~1.5rem height) maps exactly to `radiusPill`.
 *  - `width: '.625rem', height: '10px'` (baseline's plus-icon size, itself
 *    inconsistent rem/px units) -> `width: '.625rem', height: '.625rem'`
 *    (10px either way; R-T9 bans raw px in spacing-adjacent properties, and
 *    a mixed-unit pair for the same visual square was clearly unintentional
 *    in the baseline).
 *  - Typed via `@xyflow/react`'s `HandleType`/`Position` instead of the
 *    baseline's untyped string props -- the baseline is plain JS and never
 *    typed this component at all.
 */
import type { CSSProperties, MouseEvent, ReactNode } from 'react';
import { memo, useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import { useTheme, type SxProps, type Theme } from '@mui/material/styles';

import { Handle, Position, useEdges, useNodeId, useReactFlow, type HandleType } from '@xyflow/react';

import { ORIENTATION, type Orientation } from '../../lib/flow-editor/constants/flowEditor.constants';
import { useNodeCardContext } from '../../lib/flow-editor/hooks/useNodeCardContext';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

export interface CustomHandleProps {
  readonly type: HandleType;
  readonly id?: string;
  readonly isConnectable?: boolean;
  readonly label?: string;
  readonly orientation?: Orientation;
  readonly isRunningPipeline?: boolean;
  readonly isPerforming?: boolean;
  readonly style?: CSSProperties;
}

interface HandleStyleParams {
  readonly theme: Theme;
  readonly orientation: Orientation;
  readonly isPerforming?: boolean | undefined;
  readonly isRunningPipeline?: boolean | undefined;
  readonly isConnectedEdgeSelected: boolean;
  readonly specifiedStyle: CSSProperties;
  readonly isExpanded: boolean | undefined;
}

function customHandleStyles({
  theme,
  orientation,
  isPerforming,
  isRunningPipeline,
  isConnectedEdgeSelected,
  specifiedStyle,
  isExpanded,
}: HandleStyleParams): {
  readonly handle: CSSProperties;
  readonly verticalLineStyles: SxProps<Theme>;
  readonly plusCircle: SxProps<Theme>;
  readonly plusIcon: CSSProperties;
} {
  const borderStyle = isPerforming ? '.125rem dashed' : '.0625rem solid';
  const borderColor =
    isPerforming || (!isRunningPipeline && isConnectedEdgeSelected)
      ? theme.vars.palette.primary.main
      : theme.vars.palette.border.flowNode;

  const handleSize = isExpanded === false ? '.75rem' : '1.5rem';
  const handlePadding = isExpanded === false ? '0rem' : `${theme.spacing(0.25)} ${theme.spacing(0.75)}`;
  const backgroundColor = isExpanded ? theme.vars.palette.background.chatBkg : theme.vars.palette.border.flowNode;

  const handle: CSSProperties =
    orientation === ORIENTATION.horizontal
      ? {
          width: handleSize,
          padding: handlePadding,
          height: 'auto',
          borderRadius: theme.vars.shape.radiusPill,
          backgroundColor,
          border: `${borderStyle} ${borderColor}`,
          display: 'flex',
          flexDirection: 'row',
          justifyContent: 'center',
          alignItems: 'center',
          zIndex: 2,
          ...specifiedStyle,
        }
      : {
          width: isExpanded ? 'auto' : handleSize,
          padding: handlePadding,
          height: handleSize,
          borderRadius: isExpanded ? theme.vars.shape.radiusPill : theme.vars.shape.radiusSm,
          backgroundColor,
          border: `${borderStyle} ${borderColor}`,
          display: 'flex',
          flexDirection: 'column',
          justifyContent: 'center',
          alignItems: 'center',
          zIndex: 2,
          ...specifiedStyle,
        };

  return {
    handle,
    verticalLineStyles: {
      position: 'absolute',
      top: '1.4375rem',
      left: '50%',
      transform: 'translateX(-50%)',
      width: '.125rem',
      height: '.3125rem',
      backgroundColor: borderColor,
      borderRadius: theme.vars.shape.radiusSm,
      zIndex: 9,
    },
    plusCircle: {
      position: 'absolute',
      top: '1.75rem',
      left: '50%',
      transform: 'translateX(-50%)',
      width: '1rem',
      height: '1rem',
      borderRadius: theme.vars.shape.radiusPill,
      backgroundColor: theme.vars.palette.background.chatBkg,
      border: `${borderStyle} ${borderColor}`,
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      color: theme.vars.palette.text.secondary,
      zIndex: 10,
      '&:hover': {
        borderColor: theme.vars.palette.primary.main,
        color: theme.vars.palette.primary.main,
      },
    },
    plusIcon: { width: '.625rem', height: '.625rem' },
  };
}

export const CustomHandle = memo(function CustomHandle(props: CustomHandleProps): ReactNode {
  const {
    type,
    id,
    isConnectable,
    label = '',
    orientation = ORIENTATION.vertical,
    isRunningPipeline,
    isPerforming,
    style: specifiedStyle = {},
  } = props;

  const theme = useTheme();
  const nodeId = useNodeId();
  const edges = useEdges();
  const finalLabel = useMemo(() => label || (type === 'source' ? 'Output' : 'Input'), [label, type]);

  const nodeCardContext = useNodeCardContext();
  const isExpanded = nodeCardContext?.isExpanded;

  const connectedEdges = useMemo(() => {
    return edges.filter(edge => {
      if (type === 'source') {
        return edge.source === nodeId && (edge.sourceHandle === id || !id || !edge.sourceHandle);
      }
      return edge.target === nodeId && (edge.targetHandle === id || !id || !edge.targetHandle);
    });
  }, [edges, type, nodeId, id]);
  const selectedEdges = useMemo(() => connectedEdges.filter(edge => edge.selected), [connectedEdges]);

  const { setEdges } = useReactFlow();

  const handleClick = useCallback(
    (event: MouseEvent<HTMLDivElement>) => {
      event.stopPropagation();

      if (connectedEdges.length !== selectedEdges.length) {
        setEdges(currentEdges =>
          currentEdges.map(edge =>
            connectedEdges.find(e => e.id === edge.id) ? { ...edge, selected: true } : { ...edge, selected: false },
          ),
        );
      } else {
        setEdges(currentEdges => currentEdges.map(edge => ({ ...edge, selected: false })));
      }
    },
    [connectedEdges, selectedEdges, setEdges],
  );

  const isConnectedEdgeSelected = useMemo(() => {
    return edges.some(edge => {
      if (!edge.selected) return false;

      if (type === 'source') {
        return (
          edge.source === nodeId &&
          (edge.sourceHandle === id || (!id && !edge.sourceHandle) || edge.sourceHandle === undefined)
        );
      }
      return (
        edge.target === nodeId &&
        (edge.targetHandle === id || (!id && !edge.targetHandle) || edge.targetHandle === undefined)
      );
    });
  }, [edges, type, nodeId, id]);

  const position = useMemo(
    () =>
      type === 'source'
        ? orientation === ORIENTATION.horizontal
          ? Position.Right
          : Position.Bottom
        : orientation === ORIENTATION.horizontal
          ? Position.Left
          : Position.Top,
    [type, orientation],
  );

  const styles = customHandleStyles({
    theme,
    orientation,
    isPerforming,
    isRunningPipeline,
    isConnectedEdgeSelected,
    specifiedStyle,
    isExpanded,
  });

  return (
    <Handle
      type={type}
      style={styles.handle}
      position={position}
      onClick={handleClick}
      {...(id !== undefined ? { id } : {})}
      {...(isConnectable !== undefined ? { isConnectable } : {})}
    >
      {isExpanded && (
        <>
          <Typography
            variant="labelSmall"
            color="text.secondary"
          >
            {finalLabel}
          </Typography>
          {type === 'source' && (
            <>
              <Box
                component="span"
                sx={styles.verticalLineStyles}
              />
              <Box
                component="span"
                sx={styles.plusCircle}
              >
                <PlusIcon style={styles.plusIcon} />
              </Box>
            </>
          )}
        </>
      )}
    </Handle>
  );
});
