import type { ReactNode } from 'react';

import { EnhancedCardToolActions } from './EnhancedCardToolActions';
import type { AgentToolAssociation } from '../lib/types';
import { useToolOptions } from './useToolOptions';

interface ToolChoicesProps {
  readonly tool: AgentToolAssociation;
  readonly projectId: string | number | undefined;
  readonly selectedTools: readonly string[] | undefined;
  readonly availableTools: readonly string[] | undefined;
  readonly disabled: boolean | undefined;
  readonly onSelectedToolsChange: (next: readonly string[]) => void;
}

/**
 * The expanded half of a toolkit card: the tools this agent may use from the
 * toolkit. Mounted only while the card is open, so the catalogue read in
 * `useToolOptions` never runs for a collapsed card (#2146).
 */
export function ToolChoices({ tool, projectId, selectedTools, availableTools, disabled, onSelectedToolsChange }: ToolChoicesProps): ReactNode {
  const toolOptions = useToolOptions(tool, projectId);
  return (
    <EnhancedCardToolActions
      toolOptions={toolOptions}
      selectedTools={selectedTools}
      availableTools={availableTools}
      showActions
      disabled={disabled}
      onSelectedToolsChange={onSelectedToolsChange}
    />
  );
}
