/**
 * The Import dialog, shared by Import on the list pages and by the blueprint
 * gallery's Import (issue 3137): the preview of a read file, the write, and
 * then what is left to do. A refusal stays in the dialog, in the server's
 * words, so the user can fix the file and try again.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { EntityExportDocument, ImportOutcome } from '../api/lifecycleApi';
import type { TeamImportTeamBody } from '../api/teamImportApi';
import { summarizeImportDocument } from '../lib/importDocument';
import { importFollowUps, importRefusalOf, type ImportFollowUps, type ImportRefusal } from '../lib/importFollowUps';
import type { PendingAvatar, ReadImportResult } from '../lib/importTypes';
import { ImportFollowUpsView } from './ImportFollowUpsView';
import { ImportPreview } from './ImportPreview';

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

export function ImportDialog({ pending, isImporting, onImport, onClose, testIdPrefix }: ImportDialogProps): ReactNode {
  const [refusal, setRefusal] = useState<ImportRefusal>();
  const [done, setDone] = useState<Done>();
  const summary = pending === undefined ? undefined : summarizeImportDocument(pending.document, pending.skipped, pending.team);

  const close = (): void => {
    setRefusal(undefined);
    setDone(undefined);
    onClose();
  };

  const confirm = async (): Promise<void> => {
    if (pending === undefined || summary === undefined) return;
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
      <ImportPreview summary={summary} refusal={refusal} testIdPrefix={testIdPrefix} />
    ) : null;

  return (
    <BaseModal
      open={pending !== undefined}
      title={t('features.agentLifecycle.import.title', 'Import')}
      data-testid={`${testIdPrefix}-import-dialog`}
      onClose={close}
      onConfirm={done === undefined ? () => void confirm() : close}
      actions={
        done === undefined
          ? { confirmText: t('features.agentLifecycle.import.confirm', 'Import'), confirming: isImporting }
          : { confirmText: t('features.agentLifecycle.import.close', 'Done') }
      }
      content={content}
    />
  );
}
