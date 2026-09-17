import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { StopIcon } from '@/shared/ui/icons/stop-icon';

import type { UserInputSendControlSlotProps, UserInputSlots } from './UserInput.types';
import type { UserInputStyles } from './UserInput.styles';
import { stopIconStyle } from './UserInput.styles';
import { UploadProgressIndicator } from './UploadProgressIndicator';

/**
 * The footer's send/stop-button area, factored out of `UserInput.tsx` for
 * the same §3.5 complexity-budget reason as `UserInputEditableArea.tsx`.
 * §3.5 props budget: 9 (grouped).
 */
export interface UserInputFooterProps {
  readonly footer: ReactNode;
  readonly showStop: boolean;
  /**
   * A17: the send control is no longer the "else" of `showStop`. A host with a
   * message queue shows BOTH while a turn is open — Stop ends the run, Send
   * queues the next message (`UserInputSendButtonConfig.keepWhileStreaming`).
   * Without a queue this is `!showStop`, which is the baseline's own either/or.
   */
  readonly showSend: boolean;
  readonly sendControl: UserInputSlots['sendControl'];
  readonly sendControlProps: UserInputSendControlSlotProps;
  readonly showLoading: boolean;
  readonly uploadProgress: number | undefined;
  readonly onStop: (() => void) | undefined;
  readonly stopButtonConfig: { readonly iconColor?: string | undefined; readonly tooltipTitle?: string | undefined } | undefined;
  readonly styles: UserInputStyles;
}

function StopButton({
  onStop,
  config,
  styles,
}: {
  readonly onStop: (() => void) | undefined;
  readonly config: UserInputFooterProps['stopButtonConfig'];
  readonly styles: UserInputStyles;
}): ReactNode {
  const label = config?.tooltipTitle ?? t('chatInput.userInput.stopGenerating', 'Stop generating');
  return (
    <Tooltip
      title={label}
      placement="top"
    >
      <Box component="span">
        <BaseBtn
          variant="icon"
          color="secondary"
          sx={styles.stopButton(config?.iconColor)}
          onClick={onStop}
          aria-label={label}
        >
          <StopIcon style={stopIconStyle} />
        </BaseBtn>
      </Box>
    </Tooltip>
  );
}

export function UserInputFooter(props: UserInputFooterProps): ReactNode {
  const { footer, showStop, showSend, sendControl, sendControlProps, showLoading, uploadProgress, onStop, stopButtonConfig, styles } = props;

  return (
    <Box sx={styles.footer}>
      {footer}
      {showStop && (
        <StopButton
          onStop={onStop}
          config={stopButtonConfig}
          styles={styles}
        />
      )}
      {showSend && (
        <Box sx={styles.sendButtonContainer}>
          {sendControl?.(sendControlProps)}
          {showLoading && <UploadProgressIndicator progress={uploadProgress} />}
        </Box>
      )}
    </Box>
  );
}
