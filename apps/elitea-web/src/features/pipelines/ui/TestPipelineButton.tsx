import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

export interface TestPipelineButtonProps {
  readonly onClick: () => void;
  readonly disabledReason?: string | undefined;
  readonly label?: string | undefined;
}

/**
 * Toolbar button that opens the on-demand Pipeline Test panel.
 * Wraps the button in a `<span>` so MUI `Tooltip` still anchors when disabled.
 */
export const TestPipelineButton = memo(function TestPipelineButton({
  onClick,
  disabledReason = '',
  label,
}: TestPipelineButtonProps): ReactNode {
  const resolvedLabel = label ?? t('features.pipelines.editorPanel.testPipeline', 'Test Pipeline');
  const isDisabled = Boolean(disabledReason);

  return (
    <Tooltip
      title={disabledReason}
      placement="top"
    >
      <Box component="span">
        <BaseBtn
          variant="elitea"
          color="primary"
          onClick={onClick}
          disabled={isDisabled}
          data-testid="pipeline-test-button"
        >
          {resolvedLabel}
        </BaseBtn>
      </Box>
    </Tooltip>
  );
});
