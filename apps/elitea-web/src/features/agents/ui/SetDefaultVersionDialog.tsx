import type { ReactNode } from 'react';

import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

/**
 * The confirm step of #147's set-default action. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/version/ui/SetDefaultVersionDialog.jsx`
 * — a warning banner that names the version, a Cancel, and a
 * "Set as a default".
 *
 * The banner copy is the baseline's own agent wording: the default version
 * is what every NEW conversation, sub-agent attachment and MCP toolkit
 * resolves to, so this is not a local preference — it changes what other
 * people get. The baseline's `entityType` switch (`agent` / `skill`) is not
 * ported: skills reach their default through a different endpoint
 * (`PATCH /skill_default_version/...`, `router.go:1816`) and no skill caller
 * exists in this app, so a second message with no reader would be
 * speculative surface.
 *
 * `errorMessage` renders INSIDE the dialog rather than being reported to the
 * page. The server refuses a foreign or missing version with a 404
 * (`repos/applications.go:667-680`), and the version the user picked is
 * still on screen behind this dialog — closing it on failure would read as
 * "done". Same reasoning `DeleteVersionDialog` records for keeping its own
 * confirm dialog open on a refusal; this app has no toast infrastructure.
 */
export interface SetDefaultVersionDialogProps {
  readonly open: boolean;
  readonly versionName: string;
  readonly confirming: boolean;
  readonly errorMessage?: string | undefined;
  readonly onClose: () => void;
  readonly onConfirm: () => void;
}

const bannerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  padding: theme.spacing(2),
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.warning.main}`,
});

const bannerIconSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.warning.main,
});

const versionNameSx: SxProps<Theme> = { fontWeight: 600 };

const errorSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'block',
  marginTop: theme.spacing(1.5),
  color: theme.vars.palette.error.main,
});

export function SetDefaultVersionDialog({
  open,
  versionName,
  confirming,
  errorMessage,
  onClose,
  onConfirm,
}: SetDefaultVersionDialogProps): ReactNode {
  return (
    <BaseModal
      open={open}
      variant="simple"
      title={t('features.agents.setDefaultVersion.title', 'Set as default?')}
      onClose={onClose}
      onConfirm={onConfirm}
      data-testid="agent-version-set-default-dialog"
      actions={{
        confirmText: t('features.agents.setDefaultVersion.confirm', 'Set as a default'),
        confirming,
      }}
      content={
        <>
          <Box sx={bannerSx}>
            <WarningAmberIcon sx={bannerIconSx} />
            <Typography variant="bodySmall">
              <Box
                component="span"
                sx={versionNameSx}
                data-testid="agent-version-set-default-name"
              >
                {versionName}
              </Box>{' '}
              {t(
                'features.agents.setDefaultVersion.warning',
                'is used whenever this agent joins a new conversation, another agent or pipeline as a toolkit, or an MCP toolkit.',
              )}
            </Typography>
          </Box>
          {errorMessage !== undefined && (
            <Typography
              role="alert"
              variant="bodySmall"
              sx={errorSx}
            >
              {errorMessage}
            </Typography>
          )}
        </>
      }
    />
  );
}
