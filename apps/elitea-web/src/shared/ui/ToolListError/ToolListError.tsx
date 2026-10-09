import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

import { BaseBtn, BUTTON_VARIANTS } from '../BaseBtn';

/**
 * The state a tool picker shows when its tool list did not load (#440).
 *
 * WHY THIS COMPONENT EXISTS. Before #381 both tool-catalogue routes answered
 * a failed database read with `200 {"tools":[],"total":0}`, so every picker
 * drew one empty list for three different causes: the toolkit offers no
 * tools, the toolkit publishes its tools at run time, or the read failed.
 * The server tells the three causes apart now. The screen must do the same,
 * so a failed read replaces the picker with this error and a retry, and an
 * empty picker keeps its one meaning.
 *
 * A deployment can turn tool discovery off. That is not a failure a retry can
 * fix, so `discoveryDisabled` swaps the message for one that says so and drops
 * the retry button.
 *
 * `testId` is a prop, not a constant, because each picker already owns a
 * distinct test id (`tool-list-error`, `loop-tool-list-error`, ...) and one
 * screen can hold more than one picker.
 */
export interface ToolListErrorProps {
  /** Reads the tool list again. Connect it to the read's own `refetch`. */
  readonly onRetry: () => void;
  /** The `data-testid` of the alert. The default is the toolkit picker id. */
  readonly testId?: string;
  /** Replaces the default sentence. Use it when the failed read is not a tool list. */
  readonly message?: string;
  /** Tool discovery is turned off on this deployment (`useToolkitTools().isDiscoveryDisabled`). Names that, and offers no retry. */
  readonly discoveryDisabled?: boolean;
}

export function ToolListError({ onRetry, testId = 'tool-list-error', message, discoveryDisabled = false }: ToolListErrorProps): ReactNode {
  if (discoveryDisabled) {
    return (
      <Alert
        severity="info"
        data-testid={testId}
      >
        {t('shared.ui.toolListError.discoveryDisabled', 'Tool discovery is turned off on this deployment. Ask an administrator to enable it.')}
      </Alert>
    );
  }
  return (
    <Alert
      severity="error"
      data-testid={testId}
      action={
        <BaseBtn
          variant={BUTTON_VARIANTS.tertiary}
          size="small"
          onClick={onRetry}
        >
          {t('shared.ui.toolListError.retry', 'Retry')}
        </BaseBtn>
      }
    >
      {message ?? t('shared.ui.toolListError.message', 'The tool list did not load. Try again.')}
    </Alert>
  );
}
