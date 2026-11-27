import type { ChangeEvent, ReactNode } from 'react';
import { useRef, useState } from 'react';

import UploadFileOutlinedIcon from '@mui/icons-material/UploadFileOutlined';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { ImportDocumentError, isImportableFileName, readImportFile, type ReadImportResult } from '../lib/importDocument';
import { ImportDialog, type ImportDialogProps } from './ImportDialog';

/**
 * Import, on the agents and pipelines list headers.
 *
 * It mirrors `features/skills`' `SkillImportButton` deliberately — hidden file
 * input, extension guard, parse, PREVIEW modal, then the write — because the
 * skills one is the shape this app already proved and the one a user has
 * already met. The two differences are the file type (`.json`, the agent
 * export format, against the skill's `.md`) and the parser.
 *
 * The preview is not decoration. The import wizard creates agents in the
 * caller's project and answers 201/207 with an errors channel; showing the
 * file's contents first is what lets someone notice they picked the wrong
 * export before it lands in their project.
 *
 * A zip with a `team.yaml` is a team (issue 3010): the preview names the team
 * and its lead, and the import creates the agents AND the team, or nothing.
 * The dialog itself (preview, refusal, and what is left to connect after the
 * import, issue 3137) is `ImportDialog`, which the blueprint gallery shares.
 */
export interface EntityImportButtonProps {
  readonly isImporting: boolean;
  readonly onImport: ImportDialogProps['onImport'];
  /** Distinguishes the agents and pipelines copies in the DOM. */
  readonly testIdPrefix: string;
  /** False on the Pipelines list: a team makes agents and a team, so it is imported from the Agents list. */
  readonly acceptsTeams?: boolean;
  /** The button's words: "Import team" on the Teams page (issue 3134); "Import" by default. */
  readonly label?: string;
  /** Draws each agent's avatar in the preview (the page's shared renderer, issue 3142). */
  readonly renderAvatar?: ImportDialogProps['renderAvatar'];
}

export function EntityImportButton({ isImporting, onImport, testIdPrefix, acceptsTeams = true, label, renderAvatar }: EntityImportButtonProps): ReactNode {
  const inputRef = useRef<HTMLInputElement>(null);
  const [pending, setPending] = useState<ReadImportResult>();
  const [error, setError] = useState<string>();

  const handleFile = async (event: ChangeEvent<HTMLInputElement>): Promise<void> => {
    const file = event.target.files?.[0];
    // Cleared BEFORE the await: picking the same file twice must fire `change`
    // again, and it does not while the input still holds that file.
    event.target.value = '';
    if (!file) return;
    if (!isImportableFileName(file.name)) {
      setError(t('features.agentLifecycle.import.onlyJson', 'Only .json, .md and .zip files can be imported.'));
      return;
    }
    try {
      const read = await readImportFile(file);
      if (read.team !== undefined && !acceptsTeams) {
        setError(t('features.agentLifecycle.import.teamOnAgents', 'This zip is a team. Import it from the Agents list.'));
        return;
      }
      setPending(read);
      setError(undefined);
    } catch (cause) {
      setError(
        cause instanceof ImportDocumentError
          ? cause.message
          : t('features.agentLifecycle.import.unreadable', 'This file could not be read.'),
      );
    }
  };

  return (
    <>
      <BaseBtn
        variant="secondary"
        startIcon={<UploadFileOutlinedIcon />}
        disabled={isImporting}
        data-testid={`${testIdPrefix}-import-button`}
        onClick={() => inputRef.current?.click()}
      >
        {label ?? t('features.agentLifecycle.import.button', 'Import')}
      </BaseBtn>
      <input
        ref={inputRef}
        hidden
        type="file"
        accept=".json,.md,.zip,application/json,text/markdown,application/zip"
        data-testid={`${testIdPrefix}-import-input`}
        onChange={(event) => {
          void handleFile(event);
        }}
      />
      {error !== undefined && (
        <Typography
          role="alert"
          variant="bodySmall"
          data-testid={`${testIdPrefix}-import-error`}
        >
          {error}
        </Typography>
      )}
      <ImportDialog
        pending={pending}
        isImporting={isImporting}
        onImport={onImport}
        onClose={() => setPending(undefined)}
        testIdPrefix={testIdPrefix}
        renderAvatar={renderAvatar}
      />
    </>
  );
}
