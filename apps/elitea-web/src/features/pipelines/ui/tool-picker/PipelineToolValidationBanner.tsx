import { memo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import type { SxProps, Theme } from '@mui/material/styles';

import { CredentialWarningBanner } from '@/entities/credential';
import { toolkitValidation } from '@/entities/toolkit';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';

import type { PickerToolEntry, ToolValidationOverride } from '../../lib/tool-picker/toolPicker.types';

const CREDENTIAL_NOT_FOUND = 'credential_not_found';
const PRIVATE_CREDENTIAL_NOT_FOUND = 'private_credential_not_found';
const CONFIGURATION_MODEL_NOT_FOUND = 'configuration_model_not_found';

export interface PipelineToolValidationBannerProps {
  readonly tool: PickerToolEntry;
  readonly projectId?: string | undefined;
  readonly validationOverride?: ToolValidationOverride | null | undefined;
  readonly createCredentialHref?: string | undefined;
}

const bannerWrapperSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.5),
});

const refusalRowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  flexWrap: 'wrap',
});

function resolveValidationMessage(override: ToolValidationOverride): string {
  if (
    override.errorType === CREDENTIAL_NOT_FOUND ||
    override.errorType === PRIVATE_CREDENTIAL_NOT_FOUND
  ) {
    return t(
      'pipelines.toolPicker.credentialConfigMismatch',
      'Your configuration does not match any available configurations.',
    );
  }
  if (override.errorType === CONFIGURATION_MODEL_NOT_FOUND && override.modelName) {
    return t(
      'pipelines.toolPicker.modelNotFound',
      'Model "{{modelName}}" is no longer available in project configurations.',
      { modelName: override.modelName },
    );
  }
  return override.message;
}

/**
 * Misconfigured-toolkit / version validation warning indicator below `PipelineToolCard`
 * (EL-7516 / #1593). Reads from `entities/toolkit`'s shared `useToolkitValidationInfo` store
 * and supports explicit host-supplied `validationOverride` (governed refusal, foreign private
 * credential, credential/model mismatch, or sub-agent validation error).
 */
export const PipelineToolValidationBanner = memo(function PipelineToolValidationBanner({
  tool,
  projectId,
  validationOverride,
  createCredentialHref = '/credentials/create-credential',
}: PipelineToolValidationBannerProps): ReactNode {
  const toolkitId = tool.tool_id ?? tool.id;
  const { toolkitValidationInfoList } = toolkitValidation.useToolkitValidationInfo({
    projectId,
    toolkitId: toolkitId !== undefined ? String(toolkitId) : undefined,
  });

  if (validationOverride) {
    if (validationOverride.isForeignPrivateCredential) {
      return (
        <Box data-testid="pipeline-tool-card-validation-warning">
          <CredentialWarningBanner
            section="credentials"
            createHref={createCredentialHref}
            {...(validationOverride.credentialId
              ? { credentialId: validationOverride.credentialId }
              : {})}
            {...(tool.type ? { credentialType: tool.type } : {})}
          />
        </Box>
      );
    }

    const message = resolveValidationMessage(validationOverride);
    return (
      <Box sx={bannerWrapperSx} data-testid="pipeline-tool-card-validation-warning">
        <Box sx={refusalRowSx}>
          <BannerMessage message={message} variant="warning" />
          {validationOverride.refusalCode ? (
            <Chip
              size="small"
              label={validationOverride.refusalCode}
              data-testid="tool-refusal-code-chip"
            />
          ) : null}
        </Box>
      </Box>
    );
  }

  if (toolkitValidationInfoList.length === 0) return null;

  const combinedMessage = toolkitValidationInfoList
    .map((entry) => entry.msg)
    .filter(Boolean)
    .join(' ');

  if (!combinedMessage) return null;

  return (
    <Box sx={bannerWrapperSx} data-testid="pipeline-tool-card-validation-warning">
      <BannerMessage message={combinedMessage} variant="warning" />
    </Box>
  );
});
