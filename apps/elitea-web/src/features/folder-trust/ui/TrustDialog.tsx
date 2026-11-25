/**
 * "Trust this folder…" (ADR-0061 decision 5a, #3062): what changes when a
 * folder runs its commands outside the sandbox, in the ADR's four
 * statements, and the person's explicit confirmation (a tick next to the
 * folder's name). When the organisation's policy does not allow trust, the
 * dialog says why and offers nothing. A task worktree follows its folder
 * and is never trusted on its own. "Trust" sends the text's version
 * (`TRUST_CONFIRMATION_VERSION`), which the host records.
 */
import { Accordion, AccordionDetails, AccordionSummary, Alert, Box, Button, Dialog, DialogActions, DialogContent, DialogTitle, FormControlLabel, Stack, Typography } from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import { useState } from 'react';

import type { TrustStatus } from '@/shared/desktop/trustIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import { blockReason, platformName, trustStatements } from '../model/trustCopy';
import { useTrustActions, useTrustIpc } from '../model/useFolderTrust';

export interface TrustDialogProps {
  open: boolean;
  /** The folder's name, shown in the title and next to the confirmation. */
  workspaceName: string;
  status: TrustStatus;
  onClose: () => void;
}

/** Why nothing can be granted here, or `null` when the person may confirm. */
function Refusal({ status }: { status: TrustStatus }): React.JSX.Element | null {
  if (!status.allowed_by_policy) {
    return (
      <Alert severity="info" data-testid="trust-dialog-blocked">
        <Typography variant="bodySmall" sx={{ fontWeight: 600 }}>
          {t('desktop.trust.blockedTitle', 'Your organisation’s policy does not allow trusted folders here.')}
        </Typography>
        <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
          {status.policy_blocks.map((block) => (
            <li key={block}>
              <Typography variant="bodySmall">{blockReason(block)}</Typography>
            </li>
          ))}
        </Box>
      </Alert>
    );
  }
  if (!status.grantable) {
    return (
      <Alert severity="info" data-testid="trust-dialog-inherited">
        {status.inherited_from === null
          ? t('desktop.trust.notGrantable', 'This folder cannot be trusted: it is inside the app’s own data.')
          : t('desktop.trust.inherited', 'This is a task worktree: it follows the trust of the folder it was made from. Trust or revoke that folder instead.')}
      </Alert>
    );
  }
  return null;
}

function errorText(code: string | undefined, message: string): string {
  if (code === 'confirmation_outdated') return t('desktop.trust.outdated', 'This warning is out of date for the desktop app. Update the app, then try again.');
  if (code === 'trust_not_allowed') return t('desktop.trust.notAllowedNow', 'Your organisation’s policy no longer allows trusted folders.');
  return message;
}

export function TrustDialog({ open, workspaceName, status, onClose }: TrustDialogProps): React.JSX.Element {
  const ipc = useTrustIpc();
  const { grant } = useTrustActions(ipc);
  const [ticked, setTicked] = useState(false);
  const [askEvery, setAskEvery] = useState(status.ask_every_command);
  const offered = status.allowed_by_policy && status.grantable;
  const failure = grant.error as { code?: string; message: string } | null;
  const close = (): void => {
    setTicked(false);
    grant.reset();
    onClose();
  };
  const confirm = (): void => grant.mutate({ workspaceId: status.workspace_id, askEveryCommand: askEvery }, { onSuccess: close });
  return (
    <Dialog open={open} onClose={close} aria-labelledby="trust-dialog-title" data-testid="trust-dialog" maxWidth="sm" fullWidth>
      <DialogTitle id="trust-dialog-title">{t('desktop.trust.title', 'Trust “{{name}}”: run its commands without a sandbox?', { name: workspaceName })}</DialogTitle>
      <DialogContent>
        <Stack spacing={1.5}>
          <Typography variant="bodyMedium">
            {t(
              'desktop.trust.intro',
              'In a trusted folder, commands run as your own processes: no sandbox, your login-shell environment and PATH, your keychain and command-line logins, and the network. File tools keep their workspace rules: writes outside the folder are still refused.',
            )}
          </Typography>
          {!status.sandbox_available && (
            <Typography variant="bodyMedium" data-testid="trust-dialog-no-sandbox">
              {t('desktop.trust.noSandbox', '{{platform}} has no sandbox to fall back to: without trust, commands here are refused or asked one by one; with it, they run as you.', { platform: platformName(status.platform) })}
            </Typography>
          )}
          <Box component="ul" data-testid="trust-dialog-statements" sx={{ m: 0, pl: 2.5 }}>
            {trustStatements().map((statement) => (
              <li key={statement}>
                <Typography variant="bodyMedium">{statement}</Typography>
              </li>
            ))}
          </Box>
          <Refusal status={status} />
          {offered && (
            <>
              <FormControlLabel
                control={<BaseCheckbox checked={ticked} onChange={(event: React.ChangeEvent<HTMLInputElement>) => setTicked(event.target.checked)} />}
                label={t('desktop.trust.confirm', 'I understand. Run the commands of “{{name}}” without a sandbox.', { name: workspaceName })}
              />
              <Accordion disableGutters elevation={0}>
                <AccordionSummary expandIcon={<ExpandMoreIcon />}>
                  <Typography variant="labelMedium">{t('desktop.trust.advanced', 'Advanced options')}</Typography>
                </AccordionSummary>
                <AccordionDetails>
                  <FormControlLabel
                    control={<BaseSwitch checked={askEvery} onChange={(event: React.ChangeEvent<HTMLInputElement>) => setAskEvery(event.target.checked)} />}
                    label={t('desktop.trust.askEvery', 'Ask for every command')}
                  />
                  <Typography variant="bodySmall" color="text.secondary">
                    {t('desktop.trust.askEveryHelp', 'Commands still run without a sandbox, but each one waits for your answer first, even one a rule or a remembered choice would allow.')}
                  </Typography>
                </AccordionDetails>
              </Accordion>
            </>
          )}
          {failure !== null && <Alert severity="error">{errorText(failure.code, failure.message)}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{offered ? t('desktop.trust.cancel', 'Cancel') : t('desktop.trust.close', 'Close')}</Button>
        {offered && (
          <Button color="warning" variant="contained" disabled={!ticked || grant.isPending} onClick={confirm} data-testid="trust-dialog-confirm">
            {t('desktop.trust.grant', 'Trust this folder')}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
