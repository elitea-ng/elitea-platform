/**
 * "Handoff settings" of one folder: what a send to the cloud leaves out on
 * top of the platform's and the project's exclusions (shown read-only for
 * context), the left-out files it includes anyway (only when the policy
 * allows it), and the environment variables it carries (only when the policy
 * harvests them; names only, values are never shown). Kept by the desktop in
 * its app data, never in the folder. The host validates what is saved: a bad
 * pattern, an include outside the folder or a bad variable name is refused
 * (`invalid_request`) and shown next to its section.
 */
import { useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { FolderHandoffSettings, HandoffIpc, HandoffSettingsAnswer } from '@/shared/desktop/handoffIpc';
import { toHandoffIpcError } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { describeHandoffError } from '../model/describeHandoffError';
import { EnvChecklist } from './EnvChecklist';

export interface HandoffSettingsDialogProps {
  open: boolean;
  onClose: () => void;
  ipc: HandoffIpc;
  workspaceId: string;
}

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const sectionSx = { display: 'flex', flexDirection: 'column', gap: 0.75 } as const;

const handoffSettingsKey = (workspaceId: string) => ['workspace', 'handoff', 'settings', workspaceId] as const;

type Section = 'exclude_patterns' | 'include' | 'env_names';

/** Which section a refusal is about (`details.field`); `undefined`: the dialog as a whole. */
function sectionOf(error: unknown): Section | undefined {
  const { details } = toHandoffIpcError(error);
  const field = typeof details === 'object' && details !== null ? (details as { field?: unknown }).field : undefined;
  return field === 'exclude_patterns' || field === 'include' || field === 'env_names' ? field : undefined;
}

function SectionError({ error, section }: { error: unknown; section: Section | undefined }): React.JSX.Element | null {
  if (error === null || error === undefined || sectionOf(error) !== section) return null;
  return (
    <Alert severity="error" data-testid="handoff-settings-error">
      {describeHandoffError(error)}
    </Alert>
  );
}

function ReadOnlyPatterns({ title, patterns, testId }: { title: string; patterns: readonly string[]; testId: string }): React.JSX.Element | null {
  if (patterns.length === 0) return null;
  return (
    <Box data-testid={testId}>
      <Typography variant="labelSmall" component="h4" sx={[{ margin: 0 }, mutedSx]}>
        {title}
      </Typography>
      <Typography variant="bodySmall" sx={[{ wordBreak: 'break-all' }, mutedSx]}>
        {patterns.join(', ')}
      </Typography>
    </Box>
  );
}

interface EditableListProps {
  entries: readonly string[];
  onChange: (entries: string[]) => void;
  addLabel: string;
  inputLabel: string;
  testId: string;
  disabled: boolean;
}

function EditableList({ entries, onChange, addLabel, inputLabel, testId, disabled }: EditableListProps): React.JSX.Element {
  const [text, setText] = useState('');
  const add = (): void => {
    const value = text.trim();
    if (value === '') return;
    if (!entries.includes(value)) onChange([...entries, value]);
    setText('');
  };
  return (
    <Box data-testid={testId} sx={sectionSx}>
      {entries.length > 0 && (
        <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
          {entries.map((entry) => (
            <Box component="li" key={entry} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
              <Typography variant="bodySmall" sx={{ flex: 1, minWidth: 0, wordBreak: 'break-all' }}>
                {entry}
              </Typography>
              <Button
                size="small"
                disabled={disabled}
                onClick={() => onChange(entries.filter((other) => other !== entry))}
                aria-label={t('workspace.handoff.settings.removeNamed', 'Remove {{entry}}', { entry })}
              >
                {t('workspace.handoff.settings.remove', 'Remove')}
              </Button>
            </Box>
          ))}
        </Box>
      )}
      <Box
        component="form"
        sx={{ display: 'flex', gap: 1, alignItems: 'center' }}
        onSubmit={(event) => {
          event.preventDefault();
          add();
        }}
      >
        <TextField size="small" label={inputLabel} value={text} disabled={disabled} onChange={(event) => setText(event.target.value)} sx={{ flex: 1 }} />
        <Button type="submit" size="small" disabled={disabled || text.trim() === ''}>
          {addLabel}
        </Button>
      </Box>
    </Box>
  );
}

function Editor({ answer, saving, error, draft, onDraft }: { answer: HandoffSettingsAnswer; saving: boolean; error: unknown; draft: FolderHandoffSettings; onDraft: (next: FolderHandoffSettings) => void }): React.JSX.Element {
  const { policy } = answer;
  return (
    <>
      <SectionError error={error} section={undefined} />
      <Box component="section" data-testid="handoff-settings-exclusions" sx={sectionSx}>
        <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
          {t('workspace.handoff.settings.exclusions', 'Left out of every send')}
        </Typography>
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.settings.exclusionsHint', 'A pattern matches at any depth; start it with / to match from the top of the folder only.')}
        </Typography>
        <ReadOnlyPatterns title={t('workspace.handoff.group.platform', 'Platform')} patterns={policy.exclusions.platform} testId="handoff-settings-platform" />
        <ReadOnlyPatterns title={t('workspace.handoff.group.project', 'Project')} patterns={policy.exclusions.project} testId="handoff-settings-project" />
        <ReadOnlyPatterns title={t('workspace.handoff.group.pathDeny', 'Hard deny')} patterns={policy.exclusions.path_deny} testId="handoff-settings-path-deny" />
        <EditableList
          testId="handoff-settings-exclude"
          entries={draft.exclude_patterns}
          onChange={(exclude_patterns) => onDraft({ ...draft, exclude_patterns })}
          inputLabel={t('workspace.handoff.settings.excludeInput', 'Pattern to leave out')}
          addLabel={t('workspace.handoff.settings.add', 'Add')}
          disabled={saving}
        />
        <SectionError error={error} section="exclude_patterns" />
      </Box>
      <Box component="section" data-testid="handoff-settings-includes" sx={sectionSx}>
        <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
          {t('workspace.handoff.settings.includes', 'Included anyway')}
        </Typography>
        {policy.allow_user_include ? (
          <>
            <Typography variant="bodySmall" sx={mutedSx}>
              {t('workspace.handoff.settings.includesHint', 'An exact path, or a pattern from the top of the folder. Included files are still checked for secrets, and a send asks you to confirm them.')}
            </Typography>
            <EditableList
              testId="handoff-settings-include"
              entries={draft.include}
              onChange={(include) => onDraft({ ...draft, include })}
              inputLabel={t('workspace.handoff.settings.includeInput', 'Path or pattern to include')}
              addLabel={t('workspace.handoff.settings.add', 'Add')}
              disabled={saving}
            />
            <SectionError error={error} section="include" />
          </>
        ) : (
          <Typography variant="bodySmall" data-testid="handoff-settings-include-off" sx={mutedSx}>
            {t('workspace.handoff.settings.includeOff', 'Your organisation does not allow including files that are left out by default.')}
          </Typography>
        )}
      </Box>
      {policy.env_harvest && (
        <Box component="section" data-testid="handoff-settings-env" sx={sectionSx}>
          <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
            {t('workspace.handoff.env.title', 'Environment variables')}
          </Typography>
          <Typography variant="bodySmall" sx={mutedSx}>
            {t('workspace.handoff.settings.envHint', 'Sent with every send from this folder: at most {{maxVars}} variables, {{maxBytes}} bytes in all.', {
              maxVars: String(policy.env_limits.max_vars),
              maxBytes: String(policy.env_limits.max_bytes),
            })}
          </Typography>
          <EnvChecklist
            offered={answer.env_offered}
            selected={draft.env_names}
            disabled={saving}
            onToggle={(name) =>
              onDraft({ ...draft, env_names: draft.env_names.includes(name) ? draft.env_names.filter((other) => other !== name) : [...draft.env_names, name] })
            }
          />
          <SectionError error={error} section="env_names" />
        </Box>
      )}
    </>
  );
}

export function HandoffSettingsDialog({ open, onClose, ipc, workspaceId }: HandoffSettingsDialogProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: handoffSettingsKey(workspaceId), queryFn: () => ipc.settingsGet(workspaceId), enabled: open, retry: false, gcTime: 0, staleTime: 0 });
  const [draft, setDraft] = useState<FolderHandoffSettings | null>(null);
  const save = useMutation({
    mutationFn: (settings: FolderHandoffSettings) => ipc.settingsSet(workspaceId, settings),
    onSuccess: (answer) => {
      queryClient.setQueryData(handoffSettingsKey(workspaceId), answer);
      void queryClient.invalidateQueries({ queryKey: ['workspace', 'handoff', 'preview', workspaceId] });
      onClose();
    },
  });
  const current = draft ?? query.data?.settings ?? null;
  return (
    <Dialog open={open} onClose={save.isPending ? undefined : onClose} aria-labelledby="handoff-settings-title" fullWidth maxWidth="sm">
      <DialogTitle id="handoff-settings-title">{t('workspace.handoff.settings.title', 'Handoff settings')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.settings.intro', 'How this folder is sent to the cloud. Kept on this computer, not in the folder.')}
        </Typography>
        {query.isError && (
          <Alert severity="error" data-testid="handoff-settings-load-error">
            {describeHandoffError(query.error)}
          </Alert>
        )}
        {query.isPending && (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
            <CircularProgress size={16} />
            <Typography variant="bodySmall" component="output">
              {t('workspace.handoff.settings.loading', 'Reading the settings…')}
            </Typography>
          </Box>
        )}
        {query.data !== undefined && current !== null && (
          <Editor
            answer={query.data}
            saving={save.isPending}
            error={save.error}
            draft={current}
            onDraft={(next) => {
              save.reset();
              setDraft(next);
            }}
          />
        )}
      </DialogContent>
      <DialogActions>
        <Button disabled={save.isPending} onClick={onClose}>
          {t('workspace.handoff.cancel', 'Cancel')}
        </Button>
        <Button variant="contained" disabled={current === null || draft === null || save.isPending} onClick={() => current !== null && save.mutate(current)}>
          {t('workspace.handoff.settings.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
