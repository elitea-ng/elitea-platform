import { memo, useCallback } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import SendIcon from '@mui/icons-material/Send';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { useVoiceFeatureFlags } from '@/shared/lib/hooks/useVoiceFeatureFlags';
import { VoicewaveIcon } from '@/shared/ui/icons/voicewave-icon';

/**
 * Chat button primitive: SendButton
 *
 * Displays either:
 *  - a send arrow (when there is text, or voice features are off)
 *  - a voice-wave icon (clickable to enter speaking mode, when the input is empty)
 *  - a speaking-mode strip (voice-wave + stop button)
 *
 * `micDisabled` (= `disabledSend || temporarilyDisabled`) gates the mic
 * control. `isDisabled` (= `disabledSend || isEmpty`) gates the send control.
 * The two must stay separate. The mic branch renders only when `isEmpty` is
 * already true. `isDisabled` there made the mic permanently non-interactive.
 * The baseline gates the mic on `disabledSend` alone (`SendButton.jsx:57`).
 *
 * The two voice flags are READ FROM THE PLATFORM (`useVoiceFeatureFlags`,
 * A14/issue 200), not hardcoded. They were the module constants
 * `VOICE_FEATURES_ENABLED = true` and
 * `VOICE_FEATURES_TEMPORARILY_DISABLED = false`. The module doc called that a
 * `shared/config` `ConfigSchema` gap. The gap is closed:
 * `GET /elitea_core/platform_settings/…` marshals both values from
 * `centry.platform_config`, and `useVoiceFeatureFlags` is the schema.
 * `enabled` hides the mic and the speaking-mode strip. `temporarilyDisabled`
 * keeps the mic visible but non-interactive, with the administrator tooltip.
 * `VoiceButton.tsx`'s `deriveVoiceButtonUiState` applies the same pairing.
 *
 * Both controls are real `IconButton`s inside a plain `span`. The `span` is the
 * `Tooltip` child on purpose. MUI cannot attach listeners to a disabled
 * element. A disabled `IconButton` as the direct child suppresses the tooltip.
 * The tooltip of that exact state carries the reason for the disabled control.
 *
 * Prop contract (injected by the composition root through `slots.sendControl`):
 *   - `isSpeakingMode`      — show the speaking-mode strip
 *   - `question`            — current input text (controls disabled state)
 *   - `disabledSend`        — disable the send action / mic control
 *   - `onEnterSpeakingMode` — toggle into speaking mode (mic click when idle)
 *   - `onExitSpeakingMode`  — toggle out of speaking mode (stop button)
 *   - `onSend`              — fire the send action
 *   - `tooltipOfSendButton` — optional tooltip override for the send action
 */
export interface SendButtonProps {
  isSpeakingMode?: boolean;
  question?: string;
  disabledSend?: boolean;
  onEnterSpeakingMode?: () => void;
  onExitSpeakingMode?: () => void;
  onSend?: () => void;
  tooltipOfSendButton?: string;
}

const voicewaveIconStyle = { width: '1rem', height: '1rem' };

/**
 * The filled accent disc both composer end-buttons render as.
 *
 * Baseline `ComponentsLib/Chat/UserInput.jsx:693-704`'s `styles.sendButton`:
 * a 1.75rem square filled `primary.main` (`#6ae8fa`), icon in
 * `icon.fill.send`, `background.button.primary.disabled` when disabled — and
 * `NewChatInput.jsx:361-365` is what supplies those three values. Measured
 * live on next.elitea.ai: `28×28`, `border-radius: 50%`,
 * `background: rgb(106,232,250)`, icon `rgb(14,19,29)`.
 *
 * Both controls used to be bare `IconButton`s with only a `color`, so the
 * composer ended in a flat outline where the production UI has a solid accent
 * button — the single most visible difference in the whole input row.
 */
function accentButtonSx(theme: Theme) {
  return {
    width: '1.75rem',
    height: '1.75rem',
    padding: 0,
    borderRadius: theme.vars.shape.radiusPill,
    backgroundColor: theme.vars.palette.primary.main,
    color: theme.vars.palette.icon.fill.send,
    '&:hover': { backgroundColor: theme.vars.palette.primary.main },
    '&.Mui-disabled': {
      opacity: 1,
      backgroundColor: theme.vars.palette.background.button.primary.disabled,
      color: theme.vars.palette.icon.fill.send,
    },
  };
}

function micTooltipTitle(temporarilyDisabled: boolean): string {
  return temporarilyDisabled
    ? t('widgets.chat.sendButton.micDisabledTooltip', 'Temporarily disabled by admin')
    : t('widgets.chat.sendButton.startSpeakingTooltip', 'Start speaking');
}

interface SendControlProps {
  readonly tooltip: string;
  readonly disabled: boolean;
  readonly onSend: () => void;
}

/**
 * The send arrow. `aria-label` repeats the tooltip text on purpose. `Tooltip`
 * spreads the child's own props after the name it injects, so an explicit
 * label wins. A different string gives a WCAG 2.5.3 label-in-name mismatch.
 */
function SendControl({ tooltip, disabled, onSend }: SendControlProps) {
  return (
    <Tooltip title={tooltip} placement="top">
      <Box component="span" sx={{ display: 'inline-flex' }}>
        <IconButton
          data-testid="chat-send-button"
          type="button"
          size="small"
          disabled={disabled}
          onClick={onSend}
          aria-label={tooltip}
          sx={accentButtonSx}
        >
          <SendIcon fontSize="small" />
        </IconButton>
      </Box>
    </Tooltip>
  );
}

interface MicControlProps {
  readonly disabled: boolean;
  readonly temporarilyDisabled: boolean;
  readonly onEnterSpeaking: () => void;
}

/** The voice-wave icon that enters speaking mode, shown while the composer is empty. */
function MicControl({ disabled, temporarilyDisabled, onEnterSpeaking }: MicControlProps) {
  const title = micTooltipTitle(temporarilyDisabled);
  return (
    <Tooltip title={title} placement="top">
      <Box component="span" sx={{ display: 'inline-flex' }}>
        <IconButton
          data-testid="chat-speaking-mode-button"
          type="button"
          size="small"
          disabled={disabled}
          onClick={onEnterSpeaking}
          aria-label={title}
          sx={accentButtonSx}
        >
          <VoicewaveIcon style={voicewaveIconStyle} />
        </IconButton>
      </Box>
    </Tooltip>
  );
}

export const SendButton = memo(
  ({
    isSpeakingMode = false,
    question = '',
    disabledSend = false,
    onEnterSpeakingMode,
    onExitSpeakingMode,
    onSend,
    tooltipOfSendButton = t('widgets.chat.sendButton.defaultTooltip', 'Send'),
  }: SendButtonProps) => {
    const voiceFlags = useVoiceFeatureFlags();
    const isEmpty = !question;
    const isDisabled = disabledSend || isEmpty;
    const micDisabled = disabledSend || voiceFlags.temporarilyDisabled;

    const handleSend = useCallback(() => {
      onSend?.();
    }, [onSend]);

    const handleEnterSpeaking = useCallback(() => {
      onEnterSpeakingMode?.();
    }, [onEnterSpeakingMode]);

    const handleExitSpeaking = useCallback(() => {
      onExitSpeakingMode?.();
    }, [onExitSpeakingMode]);

    // Voice off platform-wide: no mic, and no speaking-mode strip either.
    // This branch sits above the `isSpeakingMode` branch on purpose. A session
    // that was already in speaking mode returns to the send arrow when the
    // operator turns the feature off. It does not keep the recording surface.
    if (!voiceFlags.enabled) {
      return <SendControl tooltip={tooltipOfSendButton} disabled={isDisabled} onSend={handleSend} />;
    }

    // Speaking mode strip
    if (isSpeakingMode) {
      return (
        <Box
          sx={(theme: Theme) => ({
            display: 'flex',
            alignItems: 'center',
            gap: 0.5,
            padding: '0.5rem 1rem',
            borderRadius: theme.vars.shape.radiusLg,
            border: '1px solid',
            borderColor: 'border.chatContinue',
          })}
        >
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: 0.5,
              color: 'primary.main',
            }}
          >
            <VoicewaveIcon style={voicewaveIconStyle} />
            <Typography component="span" variant="bodyMedium" color="text.secondary">
              {t('widgets.chat.sendButton.speakingLabel', 'Speaking...')}
            </Typography>
          </Box>
          <Tooltip title={t('widgets.chat.sendButton.stopSpeakingTitle', 'Stop speaking')}>
            <IconButton
              size="small"
              onClick={handleExitSpeaking}
              sx={{ color: 'text.secondary' }}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        </Box>
      );
    }

    // Idle, empty input: show the voice-wave icon to enter speaking mode.
    if (isEmpty) {
      return (
        <MicControl
          disabled={micDisabled}
          temporarilyDisabled={voiceFlags.temporarilyDisabled}
          onEnterSpeaking={handleEnterSpeaking}
        />
      );
    }

    // Active state: send button (has text).
    return <SendControl tooltip={tooltipOfSendButton} disabled={isDisabled} onSend={handleSend} />;
  },
);

SendButton.displayName = 'SendButton';
