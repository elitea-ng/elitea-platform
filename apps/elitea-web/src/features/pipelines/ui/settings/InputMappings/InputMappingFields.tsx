import type { ReactNode } from 'react';
import { memo } from 'react';

import { capitalizeFirstChar } from '@/shared/lib/string';

import { getInputMappingFieldInfo } from '../../../lib/flow-editor/constants/nodeFieldInfo.constants';
import * as FlowEditorHelpers from '../../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlInputMappingEntry } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import { useInputOptions } from '../../../lib/flow-editor/hooks/useInputOptions';
import { InputMappingItem, type OnChangeInputMapping } from './InputMappingItem';

type InputMappingEntry = YamlInputMappingEntry & { readonly title?: string };

const DEFAULT_DATA_TYPE = 'string';

function getMappingValue(
  key: string,
  values: Readonly<Record<string, YamlInputMappingEntry>>,
  mappingInfo: Readonly<Record<string, YamlInputMappingEntry>>,
  dataType: string,
): unknown {
  if (values[key]?.value !== undefined) return values[key].value;
  if (mappingInfo[key]?.value !== undefined) return mappingInfo[key].value;
  return dataType === 'array' ? [] : '';
}

function resolveFieldTooltip(nodeType: string | undefined, key: string, infoTooltip: string | undefined): string | undefined {
  return nodeType ? getInputMappingFieldInfo(nodeType, key, infoTooltip) : infoTooltip;
}

function resolveFieldSpec(
  key: string,
  inputMapping: Readonly<Record<string, InputMappingEntry>>,
  mappingInfo: Readonly<Record<string, YamlInputMappingEntry>>,
  values: Readonly<Record<string, YamlInputMappingEntry>>,
  inputOptions: ReturnType<typeof useInputOptions>,
  nodeType: string | undefined,
) {
  const mapping = inputMapping[key];
  const info = mappingInfo[key];
  const type = values[key]?.type ?? info?.type ?? mapping?.type;
  const dataType = info?.data_type ?? DEFAULT_DATA_TYPE;
  const variableName = mapping?.title ?? capitalizeFirstChar(key.replaceAll('_', ' '));
  return {
    variableName,
    type,
    dataType,
    value: getMappingValue(key, values, mappingInfo, dataType),
    enumList: FlowEditorHelpers.getEnumList(type, info?.enum, inputOptions),
    tooltip: resolveFieldTooltip(nodeType, key, info?.tooltip),
  };
}

export interface InputMappingFieldsProps {
  readonly keys: readonly string[];
  readonly input_mapping: Readonly<Record<string, InputMappingEntry>>;
  readonly mappingInfo?: Readonly<Record<string, YamlInputMappingEntry>> | undefined;
  readonly defaultValues?: Readonly<Record<string, unknown>> | undefined;
  readonly values?: Readonly<Record<string, YamlInputMappingEntry>> | undefined;
  readonly onChangeMapping: OnChangeInputMapping;
  readonly disabled?: boolean | undefined;
  readonly nodeType?: string | undefined;
}

/**
 * Renders `InputMappingItem` rows for a chosen list of keys without the legacy nested accordion,
 * allowing node panels to place required parameters in Basic and optional parameters in Advanced.
 */
export const InputMappingFields = memo(function InputMappingFields({
  keys,
  input_mapping,
  mappingInfo = {},
  defaultValues = {},
  values = {},
  onChangeMapping,
  disabled,
  nodeType,
}: InputMappingFieldsProps): ReactNode {
  const inputOptions = useInputOptions();

  const presentKeys = keys.filter((key) => Object.hasOwn(input_mapping, key));
  if (presentKeys.length === 0) return null;

  return (
    <>
      {presentKeys.map((key) => {
        const spec = resolveFieldSpec(key, input_mapping, mappingInfo, values, inputOptions, nodeType);
        return (
          <InputMappingItem
            key={key}
            variableName={spec.variableName}
            variable={key}
            type={spec.type}
            dataType={spec.dataType}
            value={spec.value}
            enumList={spec.enumList}
            onChangeMapping={onChangeMapping}
            disabled={disabled}
            tooltip={spec.tooltip}
            defaultValues={defaultValues}
            mappingInfo={mappingInfo}
          />
        );
      })}
    </>
  );
});
