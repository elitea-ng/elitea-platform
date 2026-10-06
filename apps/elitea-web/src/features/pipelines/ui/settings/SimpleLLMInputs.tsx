import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import { SimpleLLMInputItem, type SimpleLLMInputMappingValue } from './SimpleLLMInputItem';

export interface SimpleLLMInputMappingSpec {
  readonly type?: string;
  readonly value?: unknown;
}

export interface SimpleLLMInputsProps {
  readonly inputMappings: Readonly<Record<string, SimpleLLMInputMappingSpec>>;
  readonly values: Readonly<Record<string, SimpleLLMInputMappingSpec | undefined>>;
  readonly onChangeMapping: (variable: string, value: SimpleLLMInputMappingValue) => void;
  readonly defaultValues: Readonly<Record<string, unknown>>;
  readonly disabled?: boolean | undefined;
  readonly enableAIAssistant?: boolean;
  readonly modelConfig?: AiAssistantLlmSettings | null;
  readonly gap?: string;
  readonly codeLanguage?: string | undefined;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * settings/SimpleLLMInputs.jsx` (unit A2h) -- a thin list wrapper over
 * `SimpleLLMInputItem`, one row per `inputMappings` key.
 */
export function SimpleLLMInputs(props: SimpleLLMInputsProps): ReactNode {
  const { inputMappings, values, onChangeMapping, defaultValues, disabled = false, enableAIAssistant = false, modelConfig = null, gap, codeLanguage } = props;

  const containerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap };

  return (
    <Box sx={containerSx}>
      {Object.keys(inputMappings).map(key => {
        const current = values[key] ?? inputMappings[key];
        return (
          <SimpleLLMInputItem
            key={key}
            variableName={key}
            variable={key}
            type={current?.type ?? 'fixed'}
            value={current?.value ?? defaultValues[key] ?? ''}
            defaultValue={defaultValues[key] ?? ''}
            onChangeMapping={onChangeMapping}
            disabled={disabled}
            enableAIAssistant={enableAIAssistant}
            modelConfig={modelConfig}
            codeLanguage={codeLanguage}
          />
        );
      })}
    </Box>
  );
}
