import type { ChangeEvent, ReactNode } from 'react';
import { useRef, useState } from 'react';

import UploadFileOutlinedIcon from '@mui/icons-material/UploadFileOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { EntityExportDocument } from '../api/lifecycleApi';
import type { TeamImportTeamBody } from '../api/teamImportApi';
import {
  ImportDocumentError,
  isImportableFileName,
  readImportFile,
  summarizeImportDocument,
  type ImportDocumentSummary,
} from '../lib/importDocument';

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
 * and its lead, and the import creates the agents AND the team, or nothing. A
 * refusal (a taken name, a missing toolkit) stays in the dialog, in the
 * server's words, so the user can fix the file and try again.
 */
export interface EntityImportButtonProps {
  readonly isImporting: boolean;
  readonly onImport: (request: { readonly document: EntityExportDocument; readonly team?: TeamImportTeamBody | undefined }) => Promise<void>;
  /** Distinguishes the agents and pipelines copies in the DOM. */
  readonly testIdPrefix: string;
  /** False on the Pipelines list: a team makes agents and a team, so it is imported from the Agents list. */
  readonly acceptsTeams?: boolean;
}

interface PendingImport {
  readonly document: EntityExportDocument;
  readonly team: TeamImportTeamBody | undefined;
  readonly summary: ImportDocumentSummary;
}

function ImportPreview({ summary, refusal, testIdPrefix }: { readonly summary: ImportDocumentSummary; readonly refusal: string | undefined; readonly testIdPrefix: string }): ReactNode {
  return (
    <Box>
      {summary.team !== undefined && (
        <Box data-testid={`${testIdPrefix}-import-team`}>
          <Typography variant="labelMedium">
            {t('features.agentLifecycle.import.teamName', 'Team: {{name}}', { name: summary.team.name })}
          </Typography>
          {summary.team.lead !== undefined && (
            <Typography variant="bodySmall">
              {t('features.agentLifecycle.import.teamLead', 'Lead: {{name}}', { name: summary.team.lead })}
            </Typography>
          )}
        </Box>
      )}
      {summary.names.map((name) => (
        <Typography
          key={name}
          variant="bodyMedium"
        >
          {name}
        </Typography>
      ))}
      <Typography variant="bodySmall">
        {t('features.agentLifecycle.import.toolkits', 'Toolkits')}: {summary.toolkitCount}
      </Typography>
      <Typography variant="bodySmall">
        {t('features.agentLifecycle.import.skills', 'Skills')}: {summary.skillCount}
      </Typography>
      {summary.skipped.length > 0 ? (
        <Box>
          <Typography variant="bodySmall">
            {t('features.agentLifecycle.import.skipped', 'Not imported (this file cannot carry them)')}:
          </Typography>
          {summary.skipped.map((item) => (
            <Typography
              key={item}
              variant="bodySmall"
            >
              {item}
            </Typography>
          ))}
        </Box>
      ) : null}
      {refusal !== undefined && (
        <Alert
          severity="error"
          data-testid={`${testIdPrefix}-import-refusal`}
        >
          {refusal}
        </Alert>
      )}
    </Box>
  );
}

export function EntityImportButton({ isImporting, onImport, testIdPrefix, acceptsTeams = true }: EntityImportButtonProps): ReactNode {
  const inputRef = useRef<HTMLInputElement>(null);
  const [pending, setPending] = useState<PendingImport>();
  const [error, setError] = useState<string>();
  const [refusal, setRefusal] = useState<string>();

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
      const { document, skipped, team } = await readImportFile(file);
      if (team !== undefined && !acceptsTeams) {
        setError(t('features.agentLifecycle.import.teamOnAgents', 'This zip is a team. Import it from the Agents list.'));
        return;
      }
      setRefusal(undefined);
      setPending({ document, team, summary: summarizeImportDocument(document, skipped, team) });
      setError(undefined);
    } catch (cause) {
      setError(
        cause instanceof ImportDocumentError
          ? cause.message
          : t('features.agentLifecycle.import.unreadable', 'This file could not be read.'),
      );
    }
  };

  const confirm = async (): Promise<void> => {
    if (!pending) return;
    setRefusal(undefined);
    try {
      await onImport({ document: pending.document, team: pending.team });
      setPending(undefined);
    } catch (cause) {
      setRefusal(serverErrorMessage(cause, t('features.agentLifecycle.import.failed', 'The import failed. Nothing was imported.')));
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
        {t('features.agentLifecycle.import.button', 'Import')}
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
      <BaseModal
        open={pending !== undefined}
        title={t('features.agentLifecycle.import.title', 'Import')}
        data-testid={`${testIdPrefix}-import-dialog`}
        onClose={() => setPending(undefined)}
        onConfirm={() => {
          void confirm();
        }}
        actions={{
          confirmText: t('features.agentLifecycle.import.confirm', 'Import'),
          confirming: isImporting,
        }}
        content={
          pending ? (
            <ImportPreview
              summary={pending.summary}
              refusal={refusal}
              testIdPrefix={testIdPrefix}
            />
          ) : null
        }
      />
    </>
  );
}
