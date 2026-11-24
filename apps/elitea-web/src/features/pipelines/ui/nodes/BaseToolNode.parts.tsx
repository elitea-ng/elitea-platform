import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { capitalizeFirstChar } from '@/shared/lib/string';

import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import type { YamlInputMappingEntry } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';
import { NodeConfigSection, NodeFieldHeading } from '../node-config-drawer/NodeConfigSection';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { AddOptionalParameterButton, type OptionalParameterOption } from '../settings/InputMappings/AddOptionalParameterButton';
import { InputMapping } from '../settings/InputMappings/InputMapping';
import { InputMappingFields, type InputMappingFieldsProps } from '../settings/InputMappings/InputMappingFields';
import { NodeSettings } from '../settings/NodeSettings';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import { PickerMcpStatus } from '../tool-picker/PickerMcpStatus';

const mcpHintSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.75),
  padding: theme.spacing(0.75, 1.25),
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.status.onModeration,
  color: theme.vars.palette.text.secondary,
  width: '100%',
  boxSizing: 'border-box',
});

export function McpDisconnectedHint({ mcp }: { readonly mcp: PickerToolEntry | undefined }): ReactNode {
  if (!mcp || mcp.online !== false) return null;
  const mcpName = mcp.name || 'MCP';
  return (
    <Box sx={mcpHintSx} data-testid="pipeline-mcp-node-disconnected-hint">
      <PickerMcpStatus mcp={mcp} data-testid="pipeline-mcp-node-status" />
      <Typography variant="bodySmall">
        {t(
          'pipelines.toolPicker.mcpDisconnected',
          'The {{name}} server is disconnected. Log in to use it.',
          { name: mcpName },
        )}
      </Typography>
    </Box>
  );
}

export interface UseToolNodeParameterPartitionArgs {
  readonly toolkit: string | undefined;
  readonly selectedTool: string;
  readonly inputMappings: Readonly<Record<string, unknown>> | undefined;
  readonly requiredInputs: readonly string[];
  readonly currentMappingValues: Readonly<Record<string, YamlInputMappingEntry>> | undefined;
}

export interface ToolNodeParameterPartition {
  readonly requiredKeys: readonly string[];
  readonly shownOptionalKeys: readonly string[];
  readonly addableOptionalParameters: readonly OptionalParameterOption[];
  readonly onRevealOptionalParameter: (key: string) => void;
}

/**
 * Splits tool input parameters into required (Basic) and optional (Advanced, revealed on demand).
 */
export function useToolNodeParameterPartition({
  toolkit,
  selectedTool,
  inputMappings,
  requiredInputs,
  currentMappingValues,
}: UseToolNodeParameterPartitionArgs): ToolNodeParameterPartition {
  const [revealedOptionalKeys, setRevealedOptionalKeys] = useState<readonly string[]>([]);

  useEffect(() => {
    setRevealedOptionalKeys([]);
  }, [toolkit, selectedTool]);

  const mappingKeys = useMemo(() => Object.keys(inputMappings ?? {}), [inputMappings]);
  const requiredKeys = useMemo(
    () => mappingKeys.filter((key) => requiredInputs.includes(key)),
    [mappingKeys, requiredInputs],
  );
  const optionalKeys = useMemo(
    () => mappingKeys.filter((key) => !requiredInputs.includes(key)),
    [mappingKeys, requiredInputs],
  );

  const shownOptionalKeys = useMemo(() => {
    const existing = currentMappingValues ?? {};
    return optionalKeys.filter((key) => Object.hasOwn(existing, key) || revealedOptionalKeys.includes(key));
  }, [optionalKeys, revealedOptionalKeys, currentMappingValues]);

  const addableOptionalParameters = useMemo<readonly OptionalParameterOption[]>(() => {
    const record = (inputMappings ?? {}) as Readonly<Record<string, { readonly title?: unknown } | undefined>>;
    return optionalKeys
      .filter((key) => !shownOptionalKeys.includes(key))
      .map((key) => {
        const rawTitle = Object.hasOwn(record, key) ? record[key]?.title : undefined;
        const label =
          typeof rawTitle === 'string' && rawTitle.trim().length > 0
            ? rawTitle
            : capitalizeFirstChar(key.replaceAll('_', ' '));
        return { key, label };
      });
  }, [inputMappings, optionalKeys, shownOptionalKeys]);

  const onRevealOptionalParameter = useCallback((key: string) => {
    setRevealedOptionalKeys((prev) => (prev.includes(key) ? prev : [...prev, key]));
  }, []);

  return { requiredKeys, shownOptionalKeys, addableOptionalParameters, onRevealOptionalParameter };
}

export interface BaseToolNodeAdvancedSectionProps {
  readonly id: string;
  readonly nodeType: string;
  readonly showStructuredOutput: boolean;
  readonly disabled: boolean;
  readonly isDeprecatedLayout: boolean;
  readonly shownOptionalKeys: readonly string[];
  readonly addableOptionalParameters: readonly OptionalParameterOption[];
  readonly onRevealOptionalParameter: (key: string) => void;
  readonly mappingProps: Omit<InputMappingFieldsProps, 'keys'>;
  readonly testIdPrefix: string;
}

export function BaseToolNodeAdvancedSection({
  id,
  nodeType,
  showStructuredOutput,
  disabled,
  isDeprecatedLayout,
  shownOptionalKeys,
  addableOptionalParameters,
  onRevealOptionalParameter,
  mappingProps,
  testIdPrefix,
}: BaseToolNodeAdvancedSectionProps): ReactNode {
  const addOptionalTooltip = getNodeFieldInfo(nodeType, NODE_FIELD_KEYS.addOptionalParameter) || undefined;
  return (
    <NodeConfigSection section="advanced" defaultExpanded={false}>
      {!isDeprecatedLayout ? (
        <>
          <InputMappingFields keys={shownOptionalKeys} {...mappingProps} />
          <AddOptionalParameterButton
            options={addableOptionalParameters}
            onAdd={onRevealOptionalParameter}
            disabled={disabled}
            tooltip={addOptionalTooltip}
            testIdPrefix={testIdPrefix}
          />
        </>
      ) : null}
      <NodeFieldHeading label={t('pipelines.flowEditor.baseToolNode.inputLabel', 'Input')} nodeType={nodeType} fieldKey={NODE_FIELD_KEYS.input} />
      <InputSelect id={id} inputFieldName="input" disabled={disabled} />
      <NodeFieldHeading label={t('pipelines.flowEditor.baseToolNode.outputLabel', 'Output')} nodeType={nodeType} fieldKey={NODE_FIELD_KEYS.output} />
      <OutputSelect id={id} label={t('pipelines.flowEditor.baseToolNode.outputLabel', 'Output')} outputFieldName="output" disabled={disabled} />
      <CommonInterruptSettings id={id} showStructuredOutput={showStructuredOutput} type={nodeType} disabled={disabled} />
      <NodeSettings id={id} type={nodeType} disabled={disabled} />
    </NodeConfigSection>
  );
}

export function BaseToolNodeBasicMappings({
  isDeprecatedLayout,
  requiredKeys,
  requiredInputs,
  mappingProps,
}: {
  readonly isDeprecatedLayout: boolean;
  readonly requiredKeys: readonly string[];
  readonly requiredInputs: readonly string[];
  readonly mappingProps: Omit<InputMappingFieldsProps, 'keys'>;
}): ReactNode {
  if (isDeprecatedLayout) {
    return (
      <InputMapping
        requiredInputs={requiredInputs}
        mappingInfo={mappingProps.mappingInfo}
        input_mapping={mappingProps.input_mapping}
        defaultValues={mappingProps.defaultValues}
        values={mappingProps.values}
        onChangeMapping={mappingProps.onChangeMapping}
        disabled={mappingProps.disabled}
      />
    );
  }
  return <InputMappingFields keys={requiredKeys} {...mappingProps} />;
}
