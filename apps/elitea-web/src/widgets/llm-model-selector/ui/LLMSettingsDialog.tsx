import { memo, useCallback, useEffect, useMemo, useState } from 'react';

import { Box, Button, Modal, Typography } from '@mui/material';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { VALIDATION_RULE, validateMaxTokens } from '@/widgets/llm-model-selector/lib/validation';
import { LLMSettings } from './LLMSettings';

interface LLMSettingsDialogProps {
  open: boolean;
  onApply: (settings: Record<string, unknown>) => void;
  onCancel: () => void;
  selectedModel?: { max_output_tokens?: number; supports_reasoning?: boolean } | null;
  llmSettings?: Record<string, unknown>;
  showWebhookSecret?: boolean;
  showStepsLimit?: boolean;
  onResetToDefaults?: (() => void) | undefined;
}

// R-T10/R-T1: radius and box-shadow colour must come from brand tokens, so
// this needs the theme — same `SxProps<Theme> = (theme) => ({...})` shape
// `BucketSidebar.tsx`'s `sidebarSx` uses. `boxShadow.default` is the exact
// token role `MuiDialog.ts`'s override already documents for this baseline
// `#FFFFFF0D` literal (R-T1).
const paperStyle: SxProps<Theme> = (theme) => ({
  position: 'absolute',
  top: '50%',
  left: '50%',
  transform: 'translate(-50%, -50%)',
  width: 480,
  maxWidth: '90vw',
  bgcolor: 'background.paper',
  borderRadius: theme.vars.shape.radiusLg,
  border: '1px solid',
  borderColor: 'divider',
  boxShadow: theme.vars.palette.boxShadow.default,
  p: 2,
});

/**
 * Modal dialog for editing LLM model settings.
 * Ported from `[fsd]/widgets/llm-model-selector/ui/LLMSettingsDialog.jsx`.
 */
export const LLMSettingsDialog = memo(
  ({
    open,
    onApply,
    onCancel,
    selectedModel,
    llmSettings = {},
    showWebhookSecret = false,
    showStepsLimit = false,
    onResetToDefaults,
  }: LLMSettingsDialogProps) => {
    const [localSettings, setLocalSettings] = useState(llmSettings);

    const onChangeLLMSettings = useCallback(
      (field: string) => (value: unknown) => {
        setLocalSettings((prev) => ({ ...prev, [field]: value }));
      },
      [],
    );

    const handleOK = useCallback(() => onApply(localSettings), [localSettings, onApply]);

    const isDisabled = useMemo(() => {
      const result = validateMaxTokens(
        (localSettings?.max_tokens ?? -1) as number,
        selectedModel ?? undefined,
      );
      return result !== VALIDATION_RULE.VALID;
    }, [localSettings?.max_tokens, selectedModel]);

    useEffect(() => {
      if (open) setLocalSettings(llmSettings);
    }, [llmSettings, open]);

    return (
      <Modal open={open} onClose={onCancel}>
        <Box sx={paperStyle}>
          <Typography
            variant="headingMedium"
            component="h2"
            sx={{ mb: 2 }}
          >
            {t('widgets.llmModelSelector.llmSettingsDialog.title', 'Model settings')}
          </Typography>
          <LLMSettings
            llmSettings={localSettings}
            model={selectedModel ?? undefined}
            onChangeLLMSettings={onChangeLLMSettings}
            showWebhookSecret={showWebhookSecret}
            showStepsLimit={showStepsLimit}
          />
          <Box sx={{ mt: 2, display: 'flex', justifyContent: 'flex-end', gap: 1 }}>
            {onResetToDefaults && (
              <Button
                variant="outlined"
                color="secondary"
                onClick={() => {
                  onResetToDefaults();
                  onCancel();
                }}
              >
                {t('widgets.llmModelSelector.llmSettingsDialog.resetToDefaultsLabel', 'Reset to defaults')}
              </Button>
            )}
            <Button
              variant="outlined"
              color="secondary"
              onClick={onCancel}
            >
              {t('widgets.llmModelSelector.llmSettingsDialog.cancelLabel', 'Cancel')}
            </Button>
            <Button
              variant="contained"
              onClick={handleOK}
              disabled={isDisabled}
            >
              {t('widgets.llmModelSelector.llmSettingsDialog.applyLabel', 'Apply')}
            </Button>
          </Box>
        </Box>
      </Modal>
    );
  },
);

LLMSettingsDialog.displayName = 'LLMSettingsDialog';
