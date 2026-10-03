/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/AgentNode.jsx` (179 lines) — unit A2f. Despite the filename, this is
 * a pure pipeline-canvas node renderer keyed off the YAML node's `type:
 * agent` — verified it does NOT import anything from `features/agents` (A1),
 * so there is no `no-sideways-features` risk here.
 *
 * `NodeCard`/`CustomHandle` (A2e) and `ToolSelect`/`InputSelect`/
 * `OutputSelect`/`CommonInterruptSettings` (A2h) landed in this shared
 * worktree while this sub-unit was in progress — imported from their real,
 * verified paths/prop shapes (re-checked against each file's own source
 * immediately before writing this version, not assumed from the baseline).
 *
 * `InputMapping` (baseline: `flow-editor/ui/settings/InputMapping.jsx`, the
 * accordion that renders one `InputMappingItem` per required/optional
 * variable) is unit A2i's `../settings/InputMappings/InputMapping.tsx`
 * (plural directory — verified against that file's own real, landed path),
 * imported below with the baseline's own prop names preserved exactly.
 * `useFunctionInputMapping`'s (A2d) own `mappingInfo`/`inputMappings` are
 * typed as plain `Record<string, unknown>` (that file's own choice, not
 * this sub-unit's file to retype); `InputMapping.tsx`'s (A2i) own props
 * want the narrower `YamlInputMappingEntry`-keyed shape. Both describe the
 * exact same real `input_mapping[key]`/`{type,value,...}` runtime object
 * this hook itself builds from `getDefaultInputMappingOfTool` (A2c) — the
 * cast below at the two call sites just recovers the type A2d's own return
 * type erased, it does not paper over an actual shape mismatch.
 *
 * DISCLOSED REDESIGN (forced, matches this batch's established
 * "ambient context -> explicit prop" convention): the baseline reads
 * `values.version_details.tools` from ambient `useFormikContext()`
 * (`AgentNode.jsx:31-32`) to (a) filter "Agent" (Application-typed) tools
 * for `ToolSelect`, and (b) detect an orphaned tool binding. This app has
 * no Formik (react-hook-form + zod). A `versionTools` prop (typed
 * `PipelineToolEntry[]` — `ToolSelect.tsx`'s own established shared type
 * for this exact `version_details.tools[]` shape, unit A2h) replaces the
 * ambient read; the caller (the not-yet-built pipeline-editor form, out of
 * this pure-node-renderer sub-unit's scope) supplies its own
 * `version_details.tools` field value, e.g. via `useWatch`.
 * `useFunctionInputMapping`'s own `VersionTool` parameter type is a
 * narrower local type (A2d) describing the same real object from a
 * different sub-unit's read surface; `PipelineToolEntry` (A2h) structurally
 * satisfies it directly (every `VersionTool` field it reads is present and
 * compatibly typed), so the same runtime array is passed to both call sites
 * with no cast needed.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useEdges, type NodeProps } from '@xyflow/react';

import { ToolTypes } from '@/entities/toolkit';
import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { batchUpdateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlInputMappingEntry } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { useFunctionInputMapping } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { InputMapping, type InputMappingProps } from '../settings/InputMappings/InputMapping';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import { ToolSelect } from '../select/ToolSelect';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { CustomHandle } from './CustomHandle';
import { NodeCard } from './BaseNode/NodeCard';

const toolkitFilter = (tool: PipelineToolEntry): boolean => tool.type === ToolTypes.application.value;
const EMPTY_TOOLKIT_PATCH: Record<string, unknown> = { toolkit_name: undefined, tool: undefined, input_mapping: undefined };
/** Typed to match `yamlNode?.input_mapping`'s own `Readonly<Record<string, YamlInputMappingEntry>>` (`pipelineFlow.types.ts`), not a bare `Record<string, unknown>` -- so `inputMappingValues = yamlNode?.input_mapping ?? EMPTY_RECORD` stays assignable to `InputMapping`'s `values` prop below without a cast. */
const EMPTY_RECORD: Readonly<Record<string, YamlInputMappingEntry>> = {};
/** Stable empty-array identity — `resolvedVersionTools = versionTools ?? EMPTY_TOOLS` avoids a fresh `[]` literal (and the resulting "changes every render" `useMemo`/`useCallback` dep warning) whenever the caller omits `versionTools`. */
const EMPTY_TOOLS: readonly PipelineToolEntry[] = [];

export interface AgentNodeProps extends NodeProps<FlowNode> {
  /** Replaces the baseline's ambient `values.version_details.tools` read — see module doc comment. */
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
}

interface AgentNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean;
}

/** Extracted purely to keep `AgentNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `ui/nodes/BaseNode/NodeCardHeader.tsx` (A2e) already uses for its own split-out pieces. */
function AgentNodeHandles(props: AgentNodeHandlesProps): ReactNode {
  const { isRunningPipeline, disabled, isSourceConnectable, isPerforming } = props;
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={isPerforming}
      />
      <CustomHandle
        type="source"
        id="source"
        isConnectable={isSourceConnectable && !isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={isPerforming}
      />
    </>
  );
}

/** Pure extraction of the baseline's `isOrphan` ternary chain — kept out of `AgentNode`'s own body purely to stay under the §3.5 complexity budget (12). */
function computeIsOrphan(boundTool: string | undefined, versionTools: readonly PipelineToolEntry[] | undefined): boolean {
  if (!boundTool) return false; // nothing bound — not an orphan
  const configuredTools = (versionTools ?? []).filter(toolkitFilter);
  return configuredTools.length === 0 || !configuredTools.some(tool => tool.name === boundTool);
}

/** Pure extraction of the baseline's `onSelectToolkit` patch-building ternaries — same complexity-budget reason as {@link computeIsOrphan}. */
function computeToolkitPatch(
  newToolkit: PipelineToolEntry,
  getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string,
): Record<string, unknown> {
  const isApplication = newToolkit.type === ToolTypes.application.value;
  return {
    toolkit_name: isApplication ? undefined : (newToolkit.toolkit_name ?? getToolkitNameFromSchema(newToolkit)),
    tool: isApplication ? newToolkit.name : undefined,
  };
}

/** `FlowEditorContext`, defaulted once — every one of `AgentNode`'s own reads becomes a plain property access with no further `?.`/`??`, to stay under the §3.5 complexity budget (12). */
const EMPTY_FLOW_EDITOR_CONTEXT: FlowEditorContextValue = {
  yamlJsonObject: {},
  setFlowNodes: () => {},
  setFlowEdges: () => {},
  setYamlJsonObject: () => {},
};

export const AgentNode = memo(function AgentNode(props: AgentNodeProps): ReactNode {
  const { id, data, selected, type: nodeType = FlowEditorConstants.PipelineNodeTypes.Agent, versionTools } = props;

  const edges = useEdges();
  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject, setYamlJsonObject, isRunningPipeline, disabled } = flowEditorContext;
  const resolvedVersionTools = versionTools ?? EMPTY_TOOLS;
  const isPerforming = Boolean(data?.isPerforming);
  const isFieldsDisabled = Boolean(isRunningPipeline) || Boolean(disabled);

  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);
  const boundTool = yamlNode?.tool;

  const isSourceConnectable = useMemo(
    () => !edges.find(edge => edge.source === id && edge.target !== FlowEditorConstants.PipelineNodeTypes.End),
    [edges, id],
  );

  const isOrphan = useMemo(() => computeIsOrphan(boundTool, resolvedVersionTools), [boundTool, resolvedVersionTools]);

  const { toolkitTypes, onChangeMapping, requiredInputs, mappingInfo, inputMappings, defaultValues } =
    useFunctionInputMapping({
      id,
      yamlJsonObject,
      setYamlJsonObject,
      // `PipelineToolEntry` (A2h) and `VersionTool` (A2d) describe the same
      // real `version_details.tools[]` entry from two sub-units' narrower
      // read surfaces — see module doc comment.
      versionTools: resolvedVersionTools,
    });

  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypes as never);

  const onSelectToolkit = useCallback(
    (newToolkit: PipelineToolEntry | null) => {
      const patch = newToolkit ? computeToolkitPatch(newToolkit, getToolkitNameFromSchema) : EMPTY_TOOLKIT_PATCH;
      batchUpdateYamlNode(id, patch, yamlJsonObject, setYamlJsonObject);
    },
    // baseline's own deps array (faithfully ported) -- FlowEditorContext's yamlJsonObject/setYamlJsonObject are not memoized upstream, matching this batch's established `useLLMInputMapping.ts` precedent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [id, yamlJsonObject, setYamlJsonObject, getToolkitNameFromSchema],
  );

  const selectedToolkitValue = boundTool ?? '';
  const inputMappingValues = yamlNode?.input_mapping ?? EMPTY_RECORD;

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject.entry_point === id}
      selected={selected}
      type={nodeType}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <AgentNodeHandles
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isSourceConnectable={isSourceConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      {isOrphan && (
        <Box sx={agentNodeStyles.orphanWarning}>
          <WarningAmberIcon fontSize="small" />
          <Typography variant="bodySmall">
            {t(
              'pipelines.flowEditor.agentNode.orphanWarning',
              'Agent not found — select a replacement or delete this node',
            )}
          </Typography>
        </Box>
      )}
      <ToolSelect
        label={t('pipelines.flowEditor.agentNode.agentLabel', 'Agent')}
        onSelectTool={onSelectToolkit}
        selectedToolkit={selectedToolkitValue}
        disabled={isFieldsDisabled}
        filterTypes={toolkitFilter}
        versionTools={resolvedVersionTools}
      />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.agentNode.inputLabel', 'Input')}
        inputFieldName="input"
        disabled={isFieldsDisabled}
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.agentNode.outputLabel', 'Output')}
        outputFieldName="output"
        disabled={isFieldsDisabled}
      />
      {!isOrphan && (
        <InputMapping
          requiredInputs={requiredInputs}
          // `useFunctionInputMapping`'s (A2d) own `Record<string, unknown>`
          // return type erases the `YamlInputMappingEntry`-keyed shape it
          // actually builds -- see module doc comment.
          input_mapping={inputMappings as InputMappingProps['input_mapping']}
          mappingInfo={mappingInfo as InputMappingProps['mappingInfo']}
          defaultValues={defaultValues}
          values={inputMappingValues}
          onChangeMapping={onChangeMapping}
          disabled={isFieldsDisabled}
        />
      )}
      <CommonInterruptSettings
        id={id}
        type={FlowEditorConstants.PipelineNodeTypes.Agent}
        disabled={isFieldsDisabled}
        showStructuredOutput={false}
      />
    </NodeCard>
  );
});

const agentNodeStyles: { readonly orphanWarning: SxProps<Theme> } = {
  orphanWarning: (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(0.75),
    padding: theme.spacing(0.75, 1.5),
    borderRadius: theme.vars.shape.radiusMd,
    backgroundColor: theme.vars.palette.status.onModeration,
    color: theme.vars.palette.text.secondary,
    width: '100%',
    boxSizing: 'border-box',
  }),
};
