/**
 * The Import dialog, shared by Import on the list pages and by the blueprint
 * gallery's Import (issue 3137): the preview of a read file, the write, and
 * then what is left to do. A refusal stays in the dialog, in the server's
 * words, so the user can fix the file and try again.
 *
 * The primary action says why it cannot run when it cannot (issue 3142): an
 * import in flight uploads the zip's avatar media first, which takes a
 * while for a team, and a button that only greys out read as broken. While
 * it runs the dialog stays open, so the person sees its outcome.
 */
import type { ReactNode } from 'react';
import { useId, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { EntityExportDocument, ImportOutcome } from '../api/lifecycleApi';
import type { TeamImportTeamBody } from '../api/teamImportApi';
import { importFollowUps, importRefusalOf, type ImportFollowUps, type ImportRefusal } from '../lib/importFollowUps';
import { summarizeImportDocument } from '../lib/importSummary';
import type { PendingAvatar, ReadImportResult } from '../lib/importTypes';
import { useImportAvatarUrls } from '../model/useImportAvatarUrls';
import { ImportFollowUpsView } from './ImportFollowUpsView';
import { ImportPreview, type RenderImportAvatar } from './ImportPreview';

interface ImportRequest {
  readonly document: EntityExportDocument;
  readonly team?: TeamImportTeamBody | undefined;
  readonly avatars?: readonly PendingAvatar[] | undefined;
}

export interface ImportDialogProps {
  /** The file read for import; the dialog is open while it is set. */
  readonly pending: ReadImportResult | undefined;
  readonly isImporting: boolean;
  /** Resolves with the server's answer, whose toolkits the follow-ups name. */
  readonly onImport: (request: ImportRequest) => Promise<ImportOutcome | void>;
  readonly onClose: () => void;
  readonly testIdPrefix: string;
  /** Draws each agent's live avatar (the page's shared renderer); absent draws monograms. */
  readonly renderAvatar?: RenderImportAvatar | undefined;
}

interface Done {
  readonly names: readonly string[];
  readonly followUps: ImportFollowUps;
}

function refusalOf(cause: unknown): ImportRefusal {
  return (
    importRefusalOf(cause) ?? {
      message: serverErrorMessage(cause, t('features.agentLifecycle.import.failed', 'The import failed. Nothing was imported.')),
      field: undefined,
    }
  );
}

function dialogTitle(isTeam: boolean): string {
  return isTeam ? t('features.agentLifecycle.import.teamTitle', 'Import team') : t('features.agentLifecycle.import.title', 'Import');
}

function confirmText(isTeam: boolean, isImporting: boolean): string {
  if (isImporting) return t('features.agentLifecycle.import.importing', 'Importing…');
  return isTeam ? t('features.agentLifecycle.import.confirmTeam', 'Import team') : t('features.agentLifecycle.import.confirm', 'Import');
}

interface PreviewActionsProps {
  readonly isTeam: boolean;
  readonly isImporting: boolean;
  readonly avatarCount: number;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
  readonly testIdPrefix: string;
}

/** Cancel and the primary Import, with the reason it cannot run said in words next to it. */
function progressText(isImporting: boolean, avatarCount: number): string | undefined {
  if (!isImporting) return undefined;
  return avatarCount > 0
    ? t('features.agentLifecycle.import.uploadingAvatars', 'Uploading {{count}} avatar files, then importing…', { count: avatarCount })
    : t('features.agentLifecycle.import.creating', 'Creating everything in the file…');
}

function PreviewActions({ isTeam, isImporting, avatarCount, onCancel, onConfirm, testIdPrefix }: PreviewActionsProps): ReactNode {
  const reasonId = useId();
  const reason = progressText(isImporting, avatarCount);
  return (
    <Box sx={actionsSx}>
      {reason !== undefined && (
        <Typography id={reasonId} component="output" aria-live="polite" variant="bodySmall" sx={reasonSx} data-testid={`${testIdPrefix}-import-reason`}>
          {reason}
        </Typography>
      )}
      <BaseBtn variant="secondary" disabled={isImporting} onClick={onCancel}>
        {t('shared.ui.baseModal.cancel', 'Cancel')}
      </BaseBtn>
      <BaseBtn
        variant="contained"
        color="primary"
        loading={isImporting}
        disabled={isImporting}
        aria-describedby={reason === undefined ? undefined : reasonId}
        data-testid={`${testIdPrefix}-import-confirm`}
        onClick={onConfirm}
      >
        {confirmText(isTeam, isImporting)}
      </BaseBtn>
    </Box>
  );
}

export function ImportDialog({ pending, isImporting, onImport, onClose, testIdPrefix, renderAvatar }: ImportDialogProps): ReactNode {
  const [refusal, setRefusal] = useState<ImportRefusal>();
  const [done, setDone] = useState<Done>();
  const summary = useMemo(() => (pending === undefined ? undefined : summarizeImportDocument(pending.document, pending.skipped, pending.team)), [pending]);
  const avatarUrls = useImportAvatarUrls(pending?.avatars, summary?.agents);

  const close = (): void => {
    // Escape and the backdrop as well: closing mid-import would hide its outcome.
    if (isImporting) return;
    setRefusal(undefined);
    setDone(undefined);
    onClose();
  };

  const confirm = async (): Promise<void> => {
    if (pending === undefined || summary === undefined || isImporting) return;
    setRefusal(undefined);
    try {
      const outcome = await onImport({ document: pending.document, team: pending.team, avatars: pending.avatars });
      setDone({ names: summary.names, followUps: importFollowUps(pending.document, outcome?.result) });
    } catch (cause) {
      setRefusal(refusalOf(cause));
    }
  };

  const content =
    done !== undefined ? (
      <ImportFollowUpsView names={done.names} followUps={done.followUps} testIdPrefix={testIdPrefix} />
    ) : summary !== undefined ? (
      <ImportPreview summary={summary} refusal={refusal} avatarUrls={avatarUrls} renderAvatar={renderAvatar} testIdPrefix={testIdPrefix} />
    ) : null;

  return (
    <BaseModal
      open={pending !== undefined}
      title={dialogTitle(summary?.team !== undefined)}
      data-testid={`${testIdPrefix}-import-dialog`}
      onClose={close}
      onConfirm={done === undefined ? () => void confirm() : close}
      actions={
        done === undefined
          ? {
              node: (
                <PreviewActions
                  isTeam={summary?.team !== undefined}
                  isImporting={isImporting}
                  avatarCount={pending?.avatars?.length ?? 0}
                  onCancel={close}
                  onConfirm={() => void confirm()}
                  testIdPrefix={testIdPrefix}
                />
              ),
            }
          : { confirmText: t('features.agentLifecycle.import.close', 'Done') }
      }
      content={content}
    />
  );
}

const actionsSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: theme.spacing(1.5), minWidth: 0 });
const reasonSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, marginRight: 'auto', minWidth: 0 });
