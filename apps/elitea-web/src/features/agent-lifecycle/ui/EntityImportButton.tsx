import type { ChangeEvent, ReactNode } from 'react';
import { useRef, useState } from 'react';

import UploadFileOutlinedIcon from '@mui/icons-material/UploadFileOutlined';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { EntityExportDocument } from '../api/lifecycleApi';
import {
  ImportDocumentError,
  parseImportDocument,
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
 */
export interface EntityImportButtonProps {
  readonly isImporting: boolean;
  readonly onImport: (document: EntityExportDocument) => Promise<void>;
  /** Distinguishes the agents and pipelines copies in the DOM. */
  readonly testIdPrefix: string;
}

export function EntityImportButton({ isImporting, onImport, testIdPrefix }: EntityImportButtonProps): ReactNode {
  const inputRef = useRef<HTMLInputElement>(null);
  const [pending, setPending] = useState<{ readonly document: EntityExportDocument; readonly summary: ImportDocumentSummary }>();
  const [error, setError] = useState<string>();

  const handleFile = async (event: ChangeEvent<HTMLInputElement>): Promise<void> => {
    const file = event.target.files?.[0];
    // Cleared BEFORE the await: picking the same file twice must fire `change`
    // again, and it does not while the input still holds that file.
    event.target.value = '';
    if (!file) return;
    if (!file.name.toLowerCase().endsWith('.json')) {
      setError(t('features.agentLifecycle.import.onlyJson', 'Only .json export files can be imported.'));
      return;
    }
    try {
      const document = parseImportDocument(await file.text());
      setPending({ document, summary: summarizeImportDocument(document) });
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
        {t('features.agentLifecycle.import.button', 'Import')}
      </BaseBtn>
      <input
        ref={inputRef}
        hidden
        type="file"
        accept=".json,application/json"
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
          if (!pending) return;
          void onImport(pending.document).then(() => setPending(undefined));
        }}
        actions={{
          confirmText: t('features.agentLifecycle.import.confirm', 'Import'),
          confirming: isImporting,
        }}
        content={
          pending ? (
            <Box>
              {pending.summary.names.map((name) => (
                <Typography
                  key={name}
                  variant="bodyMedium"
                >
                  {name}
                </Typography>
              ))}
              <Typography variant="bodySmall">
                {t('features.agentLifecycle.import.toolkits', 'Toolkits')}: {pending.summary.toolkitCount}
              </Typography>
              <Typography variant="bodySmall">
                {t('features.agentLifecycle.import.skills', 'Skills')}: {pending.summary.skillCount}
              </Typography>
            </Box>
          ) : null
        }
      />
    </>
  );
}
