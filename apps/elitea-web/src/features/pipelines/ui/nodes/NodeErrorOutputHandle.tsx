/**
 * The error output handle (5b-1, PLAN §3.9): shown only while the node's
 * settings say `on_error: error_output`, styled like the Router's default
 * output handle. Its edge is `settings.error_transition` (C6).
 */
import type { ReactNode } from 'react';
import { useContext } from 'react';

import { t } from '@/shared/i18n';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { ERROR_OUTPUT_HANDLE } from '../../lib/flow-editor/helpers/nodeErrorRoute.helpers';
import { readNodeSettings } from '../../lib/nodeSettings.helpers';
import { CustomHandle } from './CustomHandle';

export interface NodeErrorOutputHandleProps {
  readonly id: string;
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isPerforming: boolean;
  /** Horizontal place; a Router keeps its default output where it is. */
  readonly left?: string;
}

export function NodeErrorOutputHandle({ id, isRunningPipeline, disabled, isPerforming, left = 'calc(50% + 3.125rem)' }: NodeErrorOutputHandleProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const node = context?.yamlJsonObject.nodes?.find((entry) => entry.id === id);
  if (readNodeSettings(node).on_error !== 'error_output') return null;
  return (
    <CustomHandle
      type="source"
      id={ERROR_OUTPUT_HANDLE}
      label={t('pipelines.nodeSettings.errorOutput', 'Error output')}
      isConnectable={!isRunningPipeline && !disabled}
      isRunningPipeline={isRunningPipeline}
      isPerforming={isPerforming}
      style={{ left }}
    />
  );
}
