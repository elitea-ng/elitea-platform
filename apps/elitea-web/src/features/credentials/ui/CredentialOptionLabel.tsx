/**
 * ui/CredentialOptionLabel.tsx — one row's label inside `CredentialsSelect`'s
 * saved-credentials list: an owner icon, the display name, the row actions
 * ("open in a new tab", and "reload" for an invalid credential), and last an
 * optional "invalid" attention indicator (#6632). Ported from
 * `apps/elitea-ui/src/[fsd]/features/credentials/ui/credential-option-label/CredentialOptionLabel.jsx`.
 * Manifest COPY-110.
 *
 * DEVIATION: no ported SVG exists for a plain "person"/"user" glyph in
 * `shared/ui/icons/**` (verified by directory listing) — `Person` from
 * `@mui/icons-material` is used instead, the same fallback pattern
 * `SingleSelect.tsx`/`CategoryFilter.tsx`/`ControlsDropdown.tsx` already
 * established for this exact class of gap (R-I1-compliant single-icon
 * import).
 */
import type { MouseEvent, ReactNode } from 'react';

import PersonIcon from '@mui/icons-material/Person';
import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { BriefcaseIcon } from '@/shared/ui/icons/briefcase-icon';
import { OpenNewIcon } from '@/shared/ui/icons/open-new-icon';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { BUTTON_VARIANTS, BaseBtn } from '@/shared/ui/BaseBtn';

export interface CredentialOptionLabelProps {
  readonly isPersonal: boolean;
  readonly label: string;
  readonly credentialUrl?: string | null;
  readonly isInvalid?: boolean;
  readonly isChecking?: boolean;
  readonly invalidMessage?: string | null;
  readonly onRevalidate?: (event: MouseEvent<HTMLButtonElement>) => void;
}

export function CredentialOptionLabel({
  isPersonal,
  label,
  credentialUrl,
  isInvalid = false,
  isChecking = false,
  invalidMessage,
  onRevalidate,
}: CredentialOptionLabelProps): ReactNode {
  const attentionLabel = invalidMessage || t('credentials.optionLabel.unavailable', 'Credential is unavailable or misconfigured');
  const openInNewTabLabel = t('credentials.optionLabel.openInNewTab', 'Open in new tab');
  const reloadLabel = t('credentials.optionLabel.reload', 'Reload and apply changes');

  return (
    <Box
      component="span"
      sx={labelContainerSx}
    >
      {isPersonal ? <PersonIcon fontSize="inherit" /> : <BriefcaseIcon />}
      <Box
        component="span"
        sx={labelTextSx}
      >
        {label}
      </Box>
      {/*
        * #6632: the row actions sit BEFORE the attention icon, which is the
        * last item in the row. The actions stay visible: a live journey
        * (`e2e/live/toolkits.indicators.spec.ts`) and keyboard users reach
        * "Reload" without a hover. Every action has a fixed box, so the row
        * keeps one height whether it carries actions or not.
        */}
      <Box
        component="span"
        data-testid="credential-option-actions"
        sx={actionsBoxSx}
      >
        {credentialUrl && (
          <Tooltip
            title={openInNewTabLabel}
            placement="top"
          >
            <BaseBtn
              aria-label={openInNewTabLabel}
              data-testid="credential-open-in-new-tab-button"
              variant={BUTTON_VARIANTS.tertiary}
              size="small"
              onMouseDown={(event) => {
                event.stopPropagation();
              }}
              onClick={(event) => {
                event.stopPropagation();
                window.open(credentialUrl, '_blank', 'noopener,noreferrer');
              }}
              sx={optionActionButtonSx}
            >
              <OpenNewIcon />
            </BaseBtn>
          </Tooltip>
        )}
        {isInvalid && (
          <Tooltip
            title={reloadLabel}
            placement="top"
          >
            <BaseBtn
              aria-label={reloadLabel}
              data-testid="credential-reload-button"
              variant={BUTTON_VARIANTS.tertiary}
              size="small"
              disabled={isChecking}
              onMouseDown={(event) => {
                event.stopPropagation();
              }}
              onClick={onRevalidate}
              sx={optionActionButtonSx}
            >
              <RefreshIcon />
            </BaseBtn>
          </Tooltip>
        )}
      </Box>
      {isInvalid && (
        <Tooltip
          title={attentionLabel}
          placement="top"
        >
          <Box
            data-testid="credential-status-indicator"
            aria-label={attentionLabel}
            sx={attentionIconBoxSx}
          >
            <AttentionIcon />
          </Box>
        </Tooltip>
      )}
    </Box>
  );
}

const labelContainerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  flex: 1,
  width: '100%',
  minHeight: '1.5rem',
});

const labelTextSx: SxProps<Theme> = {
  flex: 1,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const actionsBoxSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  flexShrink: 0,
});

const optionActionButtonSx: SxProps<Theme> = {
  padding: '0.125rem',
  flexShrink: 0,
  minWidth: '1.5rem',
  width: '1.5rem',
  height: '1.5rem',
  '& svg': { width: '0.875rem', height: '0.875rem' },
};

const attentionIconBoxSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  flexShrink: 0,
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.icon.fill.attention,
  '& svg': { width: '0.875rem', height: '0.875rem' },
});
