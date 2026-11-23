/**
 * Dual-mode `NodeCard` (Wave U2 Part A):
 * - On the canvas (`!NodeConfigPanelContext`): renders the compact view-only
 *   `NodeCardView` (header, 2-line summary, `< 0.6` zoom mini-card, status
 *   icon/tooltip driven by admission issues, and handles including
 *   `NodeErrorOutputHandle`).
 * - Inside the 400px `NodeConfigPanel` (`NodeConfigPanelContext` present):
 *   renders `TriggerTypeSelector` (for entrypoints), `NodeAdmissionIssues`,
 *   and the node's form `children` (wrapped in a `Basic` `NodeConfigSection`
 *   for legacy/deprecated layouts).
 */
import type { ReactNode } from 'react';
import { memo, useContext, useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { useStore, type ReactFlowState } from '@xyflow/react';

import {
  FlowEditorContext,
  NodeCardContext,
  NodeConfigPanelContext,
} from '../../../lib/flow-editor/flowEditorContext';
import { NodeCardView } from '../../node-card-v2/NodeCardV2';
import { isMiniCardZoom } from '../../node-card-v2/nodeCardV2.helpers';
import { NodeConfigSection } from '../../node-config-drawer/NodeConfigSection';
import { NodeAdmissionIssues } from '../../settings/NodeAdmissionIssues';
import { TriggerTypeSelector, type TriggerTypeSelectorProps } from '../../settings/TriggerTypeSelector';
import { useNodeAdmissionIssues } from '../../settings/useGraphAdmission';

export interface NodeCardProps {
  readonly name: string;
  readonly isEntrypoint: boolean;
  readonly children?: ReactNode;
  readonly selected?: boolean;
  readonly isConditionNode?: boolean;
  readonly handles?: (isExpanded: boolean) => ReactNode;
  readonly isPerforming?: boolean;
  readonly type: string;
  readonly id: string;
  readonly requiredInputs?: readonly string[] | undefined;
  readonly toolNames?: readonly string[];
  readonly onDuplicateName?: (message: string) => void;
  readonly triggerProps?: Omit<TriggerTypeSelectorProps, 'disabled'>;
}

const panelBodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  width: '100%',
};

const selectIsMiniCard = (state: ReactFlowState): boolean => isMiniCardZoom(state.transform[2]);

export const NodeCard = memo(function NodeCard(props: NodeCardProps): ReactNode {
  const {
    name,
    isEntrypoint,
    children,
    selected,
    handles,
    isPerforming,
    type,
    id,
    requiredInputs,
    triggerProps,
  } = props;

  const flowEditorContext = useContext(FlowEditorContext);
  const panelContext = useContext(NodeConfigPanelContext);
  const isMiniCard = useStore(selectIsMiniCard);
  const admissionIssues = useNodeAdmissionIssues(id);

  const issueMessages = useMemo(
    () => admissionIssues.map((issue) => issue.message),
    [admissionIssues],
  );

  const yamlNode = useMemo(
    () => flowEditorContext?.yamlJsonObject.nodes?.find((node) => node.id === id),
    [flowEditorContext?.yamlJsonObject.nodes, id],
  );

  const isExpanded = panelContext ? true : !isMiniCard;
  const cardContextValue = useMemo(() => ({ isExpanded }), [isExpanded]);

  if (!flowEditorContext) {
    return null;
  }

  const isRunningPipeline = Boolean(flowEditorContext.isRunningPipeline);
  const disabled = Boolean(flowEditorContext.disabled);

  if (panelContext) {
    return (
      <NodeCardContext.Provider value={cardContextValue}>
        <Box sx={panelBodySx}>
          {isEntrypoint && (
            <TriggerTypeSelector
              {...triggerProps}
              disabled={isRunningPipeline || disabled}
            />
          )}
          <NodeAdmissionIssues nodeId={id} />
          {panelContext.isLegacyLayout ? (
            <NodeConfigSection section="basic">{children}</NodeConfigSection>
          ) : (
            children
          )}
        </Box>
      </NodeCardContext.Provider>
    );
  }

  return (
    <NodeCardContext.Provider value={cardContextValue}>
      <NodeCardView
        id={id}
        name={name}
        type={type}
        isEntrypoint={isEntrypoint}
        selected={selected}
        isPerforming={isPerforming}
        isRunningPipeline={isRunningPipeline}
        isMiniCard={isMiniCard}
        yamlNode={yamlNode}
        requiredInputs={requiredInputs}
        issues={issueMessages}
        handles={handles}
      />
    </NodeCardContext.Provider>
  );
});
