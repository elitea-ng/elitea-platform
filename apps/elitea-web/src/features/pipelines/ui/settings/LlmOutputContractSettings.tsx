import type { ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import type { OutputContract } from '@/shared/api/generated/model';
import { OutputContractField, readOutputContract } from '@/shared/ui/OutputContractField';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';

/**
 * The output contract of a pipeline LLM node (ADR-0043 decision 2, #1500).
 * It is the node's `output_contract` YAML key, a mapping of the same shape as
 * an agent's `meta.output_contract`. None removes the key.
 *
 * Not shown when the node has structured output on: the server refuses a
 * contract there (OUTPUT_CONTRACT_INVALID, reason structured_output), and
 * `CommonInterruptSettings` removes the key when structured output is turned on.
 */
export interface LlmOutputContractSettingsProps {
  readonly id: string;
  readonly disabled?: boolean | undefined;
}

export function LlmOutputContractSettings({ id, disabled }: LlmOutputContractSettingsProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const yamlJsonObject = context?.yamlJsonObject;
  const setYamlJsonObject = context?.setYamlJsonObject;
  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find((node) => node.id === id), [id, yamlJsonObject]);

  const onChange = useCallback(
    (next: OutputContract | null) => {
      if (!setYamlJsonObject) return;
      FlowEditorHelpers.batchUpdateYamlNode(id, { output_contract: next ?? undefined }, yamlJsonObject, setYamlJsonObject);
    },
    [id, yamlJsonObject, setYamlJsonObject],
  );

  if (yamlNode === undefined || yamlNode.structured_output === true) return null;
  return (
    <OutputContractField
      value={readOutputContract(yamlNode['output_contract'])}
      onChange={onChange}
      disabled={disabled}
      testIdPrefix="llm-node-output-contract"
    />
  );
}
