/**
 * The review step of a take back: what the check found (the folder as sent,
 * a clean merge, or conflicts), which cloud turn is taken back, what is
 * written, and what is never written here and why. Nothing is written yet.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { ApplyResult, HandoffChangedFile, Preflight, SkipReason, SkippedPath, TakeBackChangedPath } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

export type RenderFiles = ((files: readonly HandoffChangedFile[], key: string) => ReactNode) | undefined;

export const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const PATHS_SHOWN = 20;

export function Paths({ title, paths, testId }: { title: string; paths: readonly string[]; testId: string }): React.JSX.Element | null {
  if (paths.length === 0) return null;
  return (
    <Box component="section" data-testid={testId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {`${title} (${String(paths.length)})`}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
        {paths.slice(0, PATHS_SHOWN).map((path) => (
          <li key={path}>
            <Typography variant="bodySmall" sx={{ wordBreak: 'break-all' }}>
              {path}
            </Typography>
          </li>
        ))}
      </Box>
      {paths.length > PATHS_SHOWN && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.pathsMore', 'and {{n}} more', { n: String(paths.length - PATHS_SHOWN) })}
        </Typography>
      )}
    </Box>
  );
}

/** A changed path as listed: a rename shows where it came from. */
export function changedLabel(change: TakeBackChangedPath): string {
  if (change.change === 'renamed' && change.from !== null) return `${change.from} → ${change.path}`;
  return change.path;
}

function skipReasonLabel(reason: SkipReason): string {
  switch (reason) {
    case 'excluded':
      return t('workspace.handoff.skip.excluded', 'kept on this computer');
    case 'path_deny':
      return t('workspace.handoff.skip.pathDeny', 'blocked by policy');
    case 'too_large':
      return t('workspace.handoff.skip.tooLarge', 'too large');
    case 'binary_conflict':
      return t('workspace.handoff.skip.binaryConflict', 'binary file changed on both sides; your version is kept');
    case 'not_a_file':
      return t('workspace.handoff.skip.notAFile', 'not a regular file');
    case 'outside_folder':
      return t('workspace.handoff.skip.outsideFolder', 'outside this folder');
  }
}

function skippedLabel(skip: SkippedPath): string {
  return `${skip.path} (${skipReasonLabel(skip.reason)})`;
}

/** The diffs of what the take back writes; without a renderer (or diffs), the changed paths. */
function ChangedInCloud({ preflight, renderFiles }: { preflight: Preflight; renderFiles: RenderFiles }): React.JSX.Element | null {
  const title = t('workspace.handoff.paths.changed', 'Changed in the cloud');
  if (renderFiles === undefined || preflight.files.length === 0) return <Paths title={title} paths={preflight.changed_paths.map(changedLabel)} testId="takeback-changed-paths" />;
  return (
    <Box data-testid="takeback-diffs" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      {renderFiles(preflight.files, `takeback-${preflight.preflight_id}`)}
      {preflight.diffs_truncated && (
        <Typography variant="bodySmall" data-testid="takeback-diffs-truncated" sx={mutedSx}>
          {t('workspace.handoff.review.truncated', 'Some diffs are not shown: they are too large to show here. The files are still written.')}
        </Typography>
      )}
    </Box>
  );
}

function Summary({ preflight }: { preflight: Preflight }): React.JSX.Element {
  const counts = { n: String(preflight.changed_paths.length), turns: String(preflight.up_to_turn) };
  switch (preflight.outcome) {
    case 'fast_path':
      return <Typography variant="bodyMedium">{t('workspace.handoff.review.fast', 'Your folder is as you left it. The cloud changed {{n}} files in {{turns}} turns.', counts)}</Typography>;
    case 'clean_merge':
      return (
        <Alert severity="info" data-testid="takeback-clean-merge">
          {t(
            'workspace.handoff.review.cleanMerge',
            'Your folder changed since you sent this thread, but not where the cloud worked. Applying keeps both: your changes and the cloud’s {{n}} changed files from {{turns}} turns.',
            counts,
          )}
        </Alert>
      );
    case 'conflicts':
    case 'error':
      return (
        <Alert severity="warning" data-testid="takeback-conflicts">
          {t(
            'workspace.handoff.review.conflicts',
            'Your folder and the cloud changed the same lines. Apply the cloud’s changes with conflict markers and resolve them by hand, open them as a separate worktree next to your folder, or discard them.',
          )}
        </Alert>
      );
  }
}

/** Which cloud turn the take back goes up to; shown when the cloud worked in more than one. */
function TurnPicker({ preflight, onPickTurn }: { preflight: Preflight; onPickTurn: (turn: number) => void }): React.JSX.Element | null {
  if (preflight.cloud_turns < 2) return null;
  const turns = Array.from({ length: preflight.cloud_turns }, (_, index) => index + 1);
  return (
    <TextField
      select
      size="small"
      label={t('workspace.handoff.turn.label', 'Take back up to')}
      value={preflight.up_to_turn}
      onChange={(event) => onPickTurn(Number(event.target.value))}
      slotProps={{ htmlInput: { 'data-testid': 'takeback-turn' } }}
      sx={{ maxWidth: 280 }}
    >
      {turns.map((turn) => (
        <MenuItem key={turn} value={turn}>
          {turn === preflight.cloud_turns
            ? t('workspace.handoff.turn.latest', 'Turn {{turn}} (latest)', { turn: String(turn) })
            : t('workspace.handoff.turn.option', 'Turn {{turn}}', { turn: String(turn) })}
        </MenuItem>
      ))}
    </TextField>
  );
}

/** Taking back fewer than all cloud turns: what happens to the turns after the chosen one. */
function TurnsAfter({ preflight }: { preflight: Preflight }): React.JSX.Element | null {
  if (preflight.up_to_turn >= preflight.cloud_turns) return null;
  return (
    <Alert severity="warning" data-testid="takeback-turns-after">
      {t(
        'workspace.handoff.turn.after',
        'Turns {{from}} to {{to}} are not taken back. They are not added to this thread’s history here, and their conversation is deleted with the cloud copy when its grace period ends. Their changes are kept as commits on a local branch.',
        { from: String(preflight.up_to_turn + 1), to: String(preflight.cloud_turns) },
      )}
    </Alert>
  );
}

/** After the take-back: where the commits of the turns not taken back are. */
export function RestBranch({ result }: { result: ApplyResult }): React.JSX.Element | null {
  if (result.rest_branch === null || result.turns_not_taken === 0) return null;
  return (
    <Typography variant="bodySmall" data-testid="takeback-rest-branch" sx={mutedSx}>
      {t('workspace.handoff.result.restBranch', 'The {{n}} later cloud turns were not taken back; their commits are on the branch {{branch}}.', {
        n: String(result.turns_not_taken),
        branch: result.rest_branch,
      })}
    </Typography>
  );
}

export interface ReviewProps {
  preflight: Preflight;
  confirmingDiscard: boolean;
  renderFiles: RenderFiles;
  onPickTurn: (turn: number) => void;
}

export function Review({ preflight, confirmingDiscard, renderFiles, onPickTurn }: ReviewProps): React.JSX.Element {
  return (
    <>
      <Summary preflight={preflight} />
      <TurnPicker preflight={preflight} onPickTurn={onPickTurn} />
      <TurnsAfter preflight={preflight} />
      <Paths title={t('workspace.handoff.paths.conflicts', 'Changed on both sides')} paths={preflight.conflict_paths} testId="takeback-conflict-paths" />
      <ChangedInCloud preflight={preflight} renderFiles={renderFiles} />
      <Paths title={t('workspace.handoff.paths.skipped', 'Never written or deleted here')} paths={preflight.skipped_paths.map(skippedLabel)} testId="takeback-skipped-paths" />
      {confirmingDiscard && (
        <Alert severity="warning" data-testid="takeback-discard-confirm">
          {t('workspace.handoff.discardConfirm', 'Discard the cloud’s changes? They are not kept anywhere on this computer. Your folder stays as it is.')}
        </Alert>
      )}
    </>
  );
}
