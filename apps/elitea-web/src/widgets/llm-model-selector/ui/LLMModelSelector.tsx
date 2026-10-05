import { memo, useCallback, useId, useRef, useState } from 'react';

import { Box, Button, ButtonGroup, Divider, Tooltip, Typography } from '@mui/material';
import { useTheme, type Theme } from '@mui/material/styles';
import BusinessIcon from '@mui/icons-material/Business';
import PublicIcon from '@mui/icons-material/Public';
import SettingsIcon from '@mui/icons-material/Settings';

import { t } from '@/shared/i18n';
import type { LLMModel, LLMModelSelectorProps, LLMSettingsValues } from '@/widgets/llm-model-selector/lib/types';

import { LLMSettingsDialog } from './LLMSettingsDialog';
import LLMModelsMenu from './LLMModelsMenu';

/** Defaulted subset of {@link LLMModelSelectorProps}, resolved outside the component body. */
type NormalizedLLMModelSelectorProps = {
  models: LLMModel[];
  disabled: boolean;
  llmSettings: LLMSettingsValues;
  showWebhookSecret: boolean;
  showStepsLimit: boolean;
  showSettingsEntry: boolean;
  modelTooltip: string;
  settingsTooltip: string;
};

/** Applies the component's default prop values. Kept outside the component to keep its complexity low. */
function resolveDefaultProps(props: LLMModelSelectorProps): NormalizedLLMModelSelectorProps {
  return {
    models: props.models ?? [],
    disabled: props.disabled ?? false,
    llmSettings: props.llmSettings ?? {},
    showWebhookSecret: props.showWebhookSecret ?? false,
    showStepsLimit: props.showStepsLimit ?? false,
    showSettingsEntry: props.showSettingsEntry ?? true,
    modelTooltip: props.modelTooltip ?? t('widgets.llmModelSelector.selector.modelTooltip', 'Select LLM Model'),
    settingsTooltip: props.settingsTooltip ?? t('widgets.llmModelSelector.selector.settingsTooltip', 'Model Settings'),
  };
}

/** Resolves the label shown for the currently selected model. */
function resolveModelDisplayName(selectedModel: LLMModel | null | undefined, fallbackLabel: string): string {
  return selectedModel?.display_name || selectedModel?.name || fallbackLabel;
}

/** Renders the shared/business icon for the selected model, or nothing when no model is selected. */
function renderModelIcon(selectedModel: LLMModel | null | undefined) {
  if (!selectedModel) return null;
  return (
    <Box component="span" sx={styles.iconWrapper}>
      {selectedModel.shared ? <PublicIcon fontSize="small" /> : <BusinessIcon fontSize="small" />}
    </Box>
  );
}

/** Resolves the settings-icon fill color: muted when settings can't be opened or the control is disabled. */
function resolveSettingsIconColor(
  theme: Theme,
  onSetLLMSettings: LLMModelSelectorProps['onSetLLMSettings'],
  disabled: boolean,
) {
  return !onSetLLMSettings || disabled ? theme.vars.palette.text.disabled : undefined;
}

/**
 * Reusable LLM Model Selector with model dropdown and optional settings.
 * Ported from `[fsd]/widgets/llm-model-selector/ui/LLMModelSelector.jsx`.
 */
const LLMModelSelector = memo(
  (props: LLMModelSelectorProps) => {
    const { selectedModel, onSelectModel, onClickSettings, onSetLLMSettings, onResetToDefaults, dataTourTargetId } =
      props;
    const {
      models,
      disabled,
      llmSettings,
      showWebhookSecret,
      showStepsLimit,
      showSettingsEntry,
      modelTooltip,
      settingsTooltip,
    } = resolveDefaultProps(props);
    const theme = useTheme();
    const anchorRef = useRef<HTMLDivElement>(null);
    const [showLLMSettings, setShowLLMSettings] = useState(false);
    const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
    // One id per selector: a page can hold several (chat, agent, index chat),
    // and the listbox names its own trigger by this id.
    const triggerId = useId();

    const handleModelMenuClick = () => setAnchorEl(anchorRef.current);
    const handleClose = () => setAnchorEl(null);

    const handleSettingsClick = useCallback(() => {
      if (onClickSettings) {
        onClickSettings();
      } else if (onSetLLMSettings) {
        setShowLLMSettings(true);
      }
    }, [onClickSettings, onSetLLMSettings]);

    const handleApplySettings = useCallback(
      (newSettings: Record<string, unknown>) => {
        if (onSetLLMSettings) onSetLLMSettings(newSettings);
        setShowLLMSettings(false);
      },
      [onSetLLMSettings],
    );

    const handleCancelSettings = useCallback(() => {
      setShowLLMSettings(false);
    }, []);

    const modelDisplayName = resolveModelDisplayName(selectedModel, 'None');
    const settingsIconColor = resolveSettingsIconColor(theme, onSetLLMSettings, disabled);

    return (
      <>
        <ButtonGroup
          variant="outlined"
          disableElevation
          disabled={disabled}
          ref={anchorRef}
          aria-label={t('widgets.llmModelSelector.selector.modelMenuAriaLabel', 'Model Selector Menu')}
          data-testid="model-selector-button"
          sx={styles.buttonGroup}
          data-tour={dataTourTargetId || undefined}
        >
          <Tooltip placement="top" title={modelTooltip}>
            <Box
              component="span"
              sx={styles.modelButtonWrapper}
            >
              <Button
                id={triggerId}
                variant="outlined"
                disabled={disabled}
                onClick={handleModelMenuClick}
                aria-haspopup="listbox"
                aria-expanded={anchorEl !== null}
                sx={styles.modelButton}
                data-testid="model-selector-name"
              >
                {renderModelIcon(selectedModel)}
                <Box
                  component="span"
                  sx={styles.modelNameWrapper}
                >
                  <Typography
                    variant="bodyMedium"
                    component="p"
                    sx={{
                      textOverflow: 'ellipsis',
                      overflow: 'hidden',
                      whiteSpace: 'nowrap',
                    }}
                  >
                    {modelDisplayName}
                  </Typography>
                </Box>
              </Button>
            </Box>
          </Tooltip>
          {showSettingsEntry && (
            <>
              <Divider orientation="vertical" flexItem />
              <Tooltip placement="top" title={settingsTooltip}>
                <Box component="span">
                  <Button
                    size="small"
                    aria-expanded={showLLMSettings ? 'true' : undefined}
                    aria-label={t('widgets.llmModelSelector.selector.settingsMenuAriaLabel', 'model settings menu')}
                    aria-haspopup="menu"
                    onClick={handleSettingsClick}
                    variant="outlined"
                    disabled={!onSetLLMSettings}
                  >
                    <SettingsIcon
                      fontSize="small"
                      sx={{
                        width: '1rem',
                        height: '1rem',
                        fill: settingsIconColor,
                      }}
                    />
                  </Button>
                </Box>
              </Tooltip>
            </>
          )}
        </ButtonGroup>

        <LLMModelsMenu
          anchorEl={anchorEl}
          onClose={handleClose}
          labelledBy={triggerId}
          models={models}
          selectedModel={selectedModel ?? null}
          onSelectModel={onSelectModel ?? (() => {})}
        />

        {onSetLLMSettings && (
          <LLMSettingsDialog
            open={showLLMSettings}
            onApply={handleApplySettings}
            onCancel={handleCancelSettings}
            selectedModel={selectedModel ?? null}
            llmSettings={llmSettings as Record<string, unknown>}
            showWebhookSecret={showWebhookSecret}
            showStepsLimit={showStepsLimit}
            onResetToDefaults={onResetToDefaults}
          />
        )}
      </>
    );
  },
);

LLMModelSelector.displayName = 'LLMModelSelector';

export default LLMModelSelector;

const styles = {
  buttonGroup: {
    maxWidth: '100%',
    minWidth: 0,
    flexShrink: 1,
  },
  // `ButtonGroup` is a flex row, and THIS is its flex item — the Tooltip's
  // required span wrapper, not the Button. Without `flexShrink` it keeps its
  // intrinsic width no matter what the Button inside says, so a long model
  // name (an `-mini`/`-preview` suffix, or a deployment's own long id)
  // widened the whole group past the composer's action row and the name
  // painted over the settings/mic/send buttons beside it. `minWidth: 0` is
  // the other half: a flex item will not shrink below its content's
  // min-content width until it is told it may.
  modelButtonWrapper: {
    minWidth: 0,
    maxWidth: '100%',
    flexShrink: 1,
    overflow: 'hidden',
    display: 'inline-block',
  },
  // `justifyContent` keeps the icon and the name packed at the start once the
  // button is wider than its content; the gap replaces the spacing the name
  // used to get from the Button's default centring.
  modelButton: {
    minWidth: 0,
    maxWidth: '100%',
    justifyContent: 'flex-start',
    gap: 0.75,
  },
  modelNameWrapper: {
    minWidth: 0,
    overflow: 'hidden',
    display: 'block',
  },
  iconWrapper: {
    width: '1rem',
    height: '1rem',
    minWidth: '1rem',
    minHeight: '1rem',
    flexShrink: 0,
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
  },
} as const;
