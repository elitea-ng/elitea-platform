/**
 * The Changes tab's details beyond a line diff: a binary or large file's size
 * before and after, the command that changed a file, and what undo does not
 * cover (files the turn's checkpoint left out, a command whose changes could
 * not be listed). Shared by the card and the panel rows.
 */
import Alert from '@mui/material/Alert';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ChangedFile, NotSaved } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

const ONE_LINE = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

/** The most file names a coverage line names before "and N more". */
const MAX_NAMED = 5;

/** The longest command a row shows; the full one is its tooltip. */
const MAX_COMMAND = 60;

/** `1.2 MB`, `340 KB`, `12 bytes` (decimal units, one decimal under 10). */
function formatSize(bytes: number): string {
  if (bytes < 1000) return t('workspace.changes.bytes', '{{n}} bytes', { n: bytes });
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit += 1;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value).toString()} ${units[unit] ?? 'TB'}`;
}

/**
 * A file the host could not diff (binary, or too large to keep), with sizes: show them in place of the line counts.
 * A text file whose diff was dropped (a stored turn's size budget) keeps its counts.
 */
export function showsSizes(file: ChangedFile): boolean {
  return file.binary === true && (file.size_before !== undefined || file.size_after !== undefined);
}

/** `1.2 MB → 1.4 MB`; an added or deleted file shows its one side. */
export function SizeChange({ file }: { file: ChangedFile }): React.JSX.Element {
  const before = file.size_before === undefined ? undefined : formatSize(file.size_before);
  const after = file.size_after === undefined ? undefined : formatSize(file.size_after);
  const text = before !== undefined && after !== undefined ? `${before} → ${after}` : (after ?? before ?? '');
  return (
    <Typography variant="bodySmall" component="span" data-testid="change-sizes" sx={(theme: Theme) => ({ flexShrink: 0, fontVariantNumeric: 'tabular-nums', color: theme.vars.palette.text.secondary })}>
      {text}
    </Typography>
  );
}

/** "by a command: `npm run build`", cut to one line; the whole command is the tooltip. */
export function CommandNote({ command }: { command: string }): React.JSX.Element {
  const shown = command.length > MAX_COMMAND ? `${command.slice(0, MAX_COMMAND - 1)}…` : command;
  return (
    <Typography variant="bodySmall" component="span" data-testid="change-command" title={command} sx={(theme: Theme) => ({ ...ONE_LINE, minWidth: 0, color: theme.vars.palette.text.metrics })}>
      {t('workspace.changes.byCommand', 'by a command: {{command}}', { command: shown })}
    </Typography>
  );
}

function named(paths: readonly string[], count: number): string {
  const shown = paths.slice(0, MAX_NAMED).join(', ');
  const more = count - Math.min(paths.length, MAX_NAMED);
  return more > 0 ? t('workspace.changes.andMore', '{{names}} and {{n}} more', { names: shown, n: more }) : shown;
}

const NOTHING_LEFT_OUT: NotSaved = { too_large: [], too_large_count: 0, placeholders: [], placeholder_count: 0 };

export interface ChangesCoverageProps {
  notSaved: NotSaved | null | undefined;
  commandsUnlisted: boolean;
}

/** One coverage line: the left-out files of one kind, named; nothing when there are none. */
function LeftOut({ testId, count, paths, text }: { testId: string; count: number; paths: readonly string[]; text: (names: string) => string }): React.JSX.Element | null {
  if (count === 0) return null;
  return (
    <Typography variant="bodySmall" component="p" data-testid={testId}>
      {text(named(paths, count))}
    </Typography>
  );
}

/** What undo does not cover for the turn, said plainly; nothing when it covers everything. */
export function ChangesCoverage({ notSaved, commandsUnlisted }: ChangesCoverageProps): React.JSX.Element | null {
  const left = notSaved ?? NOTHING_LEFT_OUT;
  if (left.too_large_count === 0 && left.placeholder_count === 0 && !commandsUnlisted) return null;
  return (
    <Alert severity="info" data-testid="changes-coverage" sx={{ marginY: 0.5 }}>
      <LeftOut
        testId="changes-not-saved"
        count={left.too_large_count}
        paths={left.too_large}
        text={(names) => t('workspace.changes.notSavedLarge', 'Too large to save for undo, so a change to them cannot be undone: {{names}}', { names })}
      />
      <LeftOut
        testId="changes-placeholders"
        count={left.placeholder_count}
        paths={left.placeholders}
        text={(names) => t('workspace.changes.notSavedCloud', 'Cloud files not downloaded, so not saved for undo: {{names}}', { names })}
      />
      {commandsUnlisted && (
        <Typography variant="bodySmall" component="p" data-testid="changes-commands-unlisted">
          {t('workspace.changes.commandsUnlisted', 'A command ran while the folder was too large to scan: files it changed may be missing from this list.')}
        </Typography>
      )}
    </Alert>
  );
}
