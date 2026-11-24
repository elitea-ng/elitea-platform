/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/HITLNode.jsx` (333 lines) — unit A2f. See `./AgentNode.tsx`'s
 * module doc comment for the shared account of not-yet-landed sibling
 * modules (`NodeCard`, `CustomHandle`, `../select/InputSelect`,
 * `../settings/{SimpleLLMInputItem,LabelWithTooltip}`) and the
 * "ambient context -> explicit prop" convention (`llmSettings` here,
 * baseline: ambient `useNodeAiAssistantConfig()`).
 *
 * `StyledTooltip` -> MUI `Tooltip` (same substitution as
 * `./DecisionNode/DecisionNodeShared.tsx`). `BasicAccordion`/`Chip.HeadingChip`
 * -> `@/shared/ui/BasicAccordion`/`@/shared/ui/HeadingChip` (already-landed
 * S1-B/S1-D ports). `Select.SingleSelect` -> `@/shared/ui/SingleSelect`
 * (already-landed S1-D port); its `onValueChange`/`showBorder`/`className`
 * props do not exist on the promoted single-value-only replacement (that
 * file's own doc comment: `onChange` not `onValueChange`, no `showBorder`/
 * `className` passthrough at all) — `onChange` is used instead, and each
 * call site is wrapped in a `Box className="nopan nodrag"` to keep the
 * React-Flow canvas-drag-suppression behaviour the baseline's `className`
 * prop provided. `error` is `string | undefined` on the promoted component
 * (not `boolean`), so the baseline's boolean `isEditRouteInvalid` becomes
 * the validation message string itself, reused as both the flag and the
 * `FormHelperText`.
 *
 * ALL derived state/handlers live in `useHITLNodeModel` (`./HITLNode.parts.tsx`)
 * — this component itself is pure hook-call-and-render, purely to stay
 * under the §3.5 complexity budget (12); see that hook's own doc comment.
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { NodeProps } from '@xyflow/react';

import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { NodeConfigSection, NodeFieldHeading } from '../node-config-drawer/NodeConfigSection';
import { LabelWithTooltip } from '../settings/InputMappings/LabelWithTooltip';
import { NodeSettings } from '../settings/NodeSettings';
import { SimpleLLMInputItem } from '../settings/SimpleLLMInputItem';
import { InputSelect } from '../select/InputSelect';
import { NodeCard } from './BaseNode/NodeCard';
import {
  HITL_ACTIONS,
  HITLNodeHandles,
  HITLRouteRow,
  computeRouteSelectDisabled,
  hitlNodeStyles,
  useHITLNodeModel,
} from './HITLNode.parts';

export interface HITLNodeProps extends NodeProps<FlowNode> {
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
}

export const HITLNode = memo(function HITLNode(props: HITLNodeProps): ReactNode {
  const { id, data, selected, llmSettings } = props;
  const isPerforming = Boolean(data?.isPerforming);
  const model = useHITLNodeModel({ id, llmSettings });
  const styles = hitlNodeStyles();

  return (
    <NodeCard
      name={id}
      isEntrypoint={model.isEntrypoint}
      selected={selected}
      type={FlowEditorConstants.PipelineNodeTypes.Hitl}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <HITLNodeHandles
          isRunningPipeline={model.isRunningPipeline}
          disabled={model.disabled}
          isTargetConnectable={model.isTargetConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      <NodeConfigSection section="basic">
        <SimpleLLMInputItem
          variableName="user_message"
          variable="user_message"
          nodeType={FlowEditorConstants.PipelineNodeTypes.Hitl}
          type={model.userMessageType}
          value={model.userMessageValue}
          defaultValue=""
          onChangeMapping={model.handleUserMessageMappingChange}
          disabled={model.simpleLLMDisabled}
          enableAIAssistant
          modelConfig={model.resolvedLlmSettings}
        />

        <BasicAccordion
          showMode="left"
          slotSx={{
            accordion: styles.accordion,
            summary: styles.accordionSummary,
            title: styles.accordionTitle,
            details: styles.accordionDetails,
          }}
          items={[
            {
              title: t('pipelines.flowEditor.hitlNode.routerMapping', 'Router mapping'),
              content: (
                <Box sx={styles.section}>
                  {HITL_ACTIONS.map(action => (
                    <HITLRouteRow
                      key={action.value}
                      action={action}
                      value={model.routes[action.value] ?? ''}
                      onChange={model.handleRouteSelectChange(action.value)}
                      options={model.routeOptionsByAction[action.value]}
                      disabled={computeRouteSelectDisabled(action.value, model.isRunningPipeline, model.disabled, model.canEditRoute)}
                      error={action.value === 'edit' ? model.routeErrorText : ''}
                    />
                  ))}
                </Box>
              ),
            },
          ]}
        />

        <Box
          sx={styles.editStateKeyRow}
          className="nopan nodrag"
        >
          <NodeFieldHeading
            label={t('pipelines.flowEditor.hitlNode.editStateKey', 'Edit state key')}
            nodeType={FlowEditorConstants.PipelineNodeTypes.Hitl}
            fieldKey={NODE_FIELD_KEYS.editStateKey}
          />
          <SingleSelect
            sx={styles.routeSelect}
            label={t('pipelines.flowEditor.hitlNode.value', 'Value')}
            value={model.editStateKey}
            onChange={model.handleEditStateKeyChange}
            options={[...model.editStateKeyOptions]}
            disabled={model.editStateKeySelectDisabled}
            error={model.routeErrorText}
          />
        </Box>
        {model.isEditRouteInvalid && (
          <Typography
            variant="bodySmall"
            color="error.main"
            sx={styles.validationText}
          >
            {model.editRouteErrorMessage}
          </Typography>
        )}
      </NodeConfigSection>
      <NodeConfigSection section="advanced" defaultExpanded={false}>
        <Box sx={styles.section}>
          <LabelWithTooltip
            title={t('pipelines.flowEditor.hitlNode.inputLabel', 'Input')}
            tooltip={model.inputSelectTooltip}
          />
          <Tooltip
            title={model.inputSelectTooltipTitle}
            placement="top"
          >
            <Box
              component="span"
              sx={styles.inputSelectTooltipWrapper}
            >
              <InputSelect
                id={id}
                label=""
                inputFieldName="input"
                disabled={model.inputSelectDisabled}
              />
            </Box>
          </Tooltip>
        </Box>
        <NodeSettings
          id={id}
          type={FlowEditorConstants.PipelineNodeTypes.Hitl}
          disabled={model.isRunningPipeline || model.disabled}
        />
      </NodeConfigSection>
    </NodeCard>
  );
});
