/**
 * The person's hooks (#1237; IPC.md, "Hooks"): the hooks of every
 * workspace and of this one, each with its event, command, sandbox and its
 * last run in the workspace's session; on/off; and each file's hooks edited
 * as JSON (checked here, then by the host's schema). The next turn runs
 * what is saved; the organisation's policy decides whether any runs.
 */
import { Alert, Box, Button, Chip, CircularProgress, Stack, TextField, Typography, type Theme } from '@mui/material';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { Hook, HookRun, HooksFileView, HooksIpc, HooksScope } from '@/shared/desktop/hooksIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import { hooksText, parseHooks } from '../model/hooksText';
import { hooksListKey } from './hooksQuery';

export interface HooksPanelProps {
  ipc: HooksIpc;
  workspaceId: string;
}

function runLabel(run: HookRun): string {
  const ms = run.duration_ms;
  switch (run.result) {
    case 'failed':
      return t('workspace.hooks.run.failed', 'Failed after {{ms}} ms', { ms });
    case 'timed_out':
      return t('workspace.hooks.run.timedOut', 'Timed out after {{ms}} ms', { ms });
    case 'deny':
      return t('workspace.hooks.run.deny', 'Refused, {{ms}} ms', { ms });
    case 'ask':
      return t('workspace.hooks.run.ask', 'Asked for approval, {{ms}} ms', { ms });
    case 'modified':
      return t('workspace.hooks.run.modified', 'Changed the input, {{ms}} ms', { ms });
    case 'annotated':
      return t('workspace.hooks.run.annotated', 'Added a note, {{ms}} ms', { ms });
    case 'ok':
      return t('workspace.hooks.run.ok', 'Ran in {{ms}} ms', { ms });
  }
}

interface HookRowProps {
  scope: HooksScope;
  hook: Hook;
  run: HookRun | undefined;
  busy: boolean;
  onToggle: (enabled: boolean) => void;
}

function HookRow({ scope, hook, run, busy, onToggle }: HookRowProps): React.JSX.Element {
  const failed = run?.result === 'failed' || run?.result === 'timed_out';
  return (
    <Box
      component="li"
      data-testid={`hook-${scope}-${hook.id}`}
      sx={(theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: 0.5, p: 1.5, border: 1, borderColor: 'divider', borderRadius: theme.vars.shape.radiusSm })}
    >
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <BaseSwitch checked={hook.enabled !== false} disabled={busy} onChange={(event: React.ChangeEvent<HTMLInputElement>) => onToggle(event.target.checked)} aria-label={t('workspace.hooks.toggle', 'Run {{id}}', { id: hook.id })} />
        <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
          {hook.id}
        </Typography>
        <Chip size="small" label={hook.event} />
        <Chip size="small" variant="outlined" label={hook.sandbox ?? DEFAULT_SANDBOX} />
        {hook.network === true && <Chip size="small" variant="outlined" label={t('workspace.hooks.network', 'Network')} />}
        {hook.fail_closed === true && <Chip size="small" variant="outlined" label={t('workspace.hooks.failClosed', 'Fail closed')} />}
      </Stack>
      <Typography variant="bodySmall" color="text.secondary" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere' }}>
        {hook.command.join(' ')}
      </Typography>
      {run !== undefined && (
        <Typography variant="bodySmall" color={failed ? 'error' : 'text.secondary'} data-testid={`hook-run-${hook.id}`}>
          {run.message === undefined ? runLabel(run) : `${runLabel(run)}: ${run.message}`}
        </Typography>
      )}
    </Box>
  );
}

/** A hook's sandbox when its entry names none (the host's default). */
const DEFAULT_SANDBOX = 'read-only';

/** The workspace's file first: its hooks replace the person's of the same id there. */
const SCOPES: readonly HooksScope[] = ['workspace', 'user'];

interface Editing {
  scope: HooksScope;
  text: string;
  problem: string | null;
}

export function HooksPanel({ ipc, workspaceId }: HooksPanelProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const key = hooksListKey(workspaceId);
  const list = useQuery({ queryKey: key, queryFn: () => ipc.list(workspaceId) });
  const [editing, setEditing] = useState<Editing | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const done = async (): Promise<void> => {
    setFailure(null);
    await queryClient.invalidateQueries({ queryKey: key });
  };
  const onError = (error: unknown): void => setFailure(toWorkspaceIpcError(error).message);
  const save = useMutation({
    mutationFn: ({ scope, hooks }: { scope: HooksScope; hooks: Hook[] }) => ipc.save(scope, hooks, workspaceId),
    onSuccess: async () => {
      setEditing(null);
      await done();
    },
    onError,
  });
  const toggle = useMutation({
    mutationFn: ({ scope, id, enabled }: { scope: HooksScope; id: string; enabled: boolean }) => ipc.setEnabled(scope, id, enabled, workspaceId),
    onSuccess: done,
    onError,
  });
  const busy = save.isPending || toggle.isPending;

  if (list.data === undefined) {
    return list.error === null ? <CircularProgress size={24} aria-label={t('workspace.hooks.loading', 'Loading hooks')} /> : <Alert severity="error">{toWorkspaceIpcError(list.error).message}</Alert>;
  }
  const view = list.data;
  const runOf = (id: string): HookRun | undefined => view.last_runs.find((run) => run.id === id);
  const submit = (scope: HooksScope, text: string): void => {
    const parsed = parseHooks(text);
    if (parsed.ok) save.mutate({ scope, hooks: parsed.hooks });
    else setEditing({ scope, text, problem: parsed.problem });
  };
  const section = (scope: HooksScope, file: HooksFileView | undefined): React.JSX.Element | null => {
    if (file === undefined) return null;
    const label = scope === 'user' ? t('workspace.hooks.scopeUser', 'Every workspace') : t('workspace.hooks.scopeWorkspace', 'This workspace');
    const open = editing?.scope === scope ? editing : null;
    return (
      <Stack spacing={1} component="section" key={scope} aria-label={label}>
        <Typography variant="labelMedium">{label}</Typography>
        {file.error !== undefined && <Alert severity="error">{t('workspace.hooks.fileRefused', 'These hooks are not used: {{message}}', { message: file.error })}</Alert>}
        {file.hooks.length === 0 ? (
          <Typography variant="bodySmall" color="text.secondary">
            {t('workspace.hooks.none', 'No hooks.')}
          </Typography>
        ) : (
          <Stack component="ul" spacing={1} sx={{ listStyle: 'none', p: 0, m: 0 }}>
            {file.hooks.map((hook) => (
              <HookRow key={hook.id} scope={scope} hook={hook} run={runOf(hook.id)} busy={busy} onToggle={(enabled) => toggle.mutate({ scope, id: hook.id, enabled })} />
            ))}
          </Stack>
        )}
        {open === null ? (
          <Box>
            <Button size="small" variant="outlined" disabled={busy} onClick={() => setEditing({ scope, text: hooksText(file.hooks), problem: null })}>
              {t('workspace.hooks.edit', 'Edit as JSON')}
            </Button>
          </Box>
        ) : (
          <Stack spacing={1}>
            <TextField
              multiline
              minRows={6}
              label={t('workspace.hooks.editorLabel', 'Hooks (JSON)')}
              value={open.text}
              onChange={(event) => setEditing({ scope, text: event.target.value, problem: null })}
              error={open.problem !== null}
              helperText={open.problem ?? t('workspace.hooks.editorHelp', 'An array of hooks: id, event, command (an argv), and optionally match, timeout_ms, sandbox, network, fail_closed, enabled.')}
              slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: 'monospace' } } }}
            />
            <Stack direction="row" spacing={1}>
              <Button size="small" variant="contained" disabled={busy} onClick={() => submit(scope, open.text)}>
                {t('workspace.hooks.save', 'Save')}
              </Button>
              <Button size="small" disabled={busy} onClick={() => setEditing(null)}>
                {t('workspace.hooks.cancel', 'Cancel')}
              </Button>
            </Stack>
          </Stack>
        )}
      </Stack>
    );
  };
  return (
    <Stack spacing={3}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'workspace.hooks.help',
          'Hooks are your own commands around the agent’s local tools: before a call (they may refuse it, ask you, or change its input), after one (they may add a note), when you send a message, and when a turn ends. Each runs on this computer under the same sandbox as the agent’s commands, with no network unless you allow it. A repository’s .elitea/hooks.json is only a proposal: nothing runs from it unless you trust it.',
        )}
      </Typography>
      {!view.allowed && (
        <Alert severity="info" data-testid="hooks-not-allowed">
          {t('workspace.hooks.notAllowed', 'Your organisation’s policy turns local commands off, so none of these run.')}
        </Alert>
      )}
      {failure !== null && <Alert severity="error">{failure}</Alert>}
      {SCOPES.map((scope) => section(scope, scope === 'user' ? view.user : view.workspace))}
    </Stack>
  );
}
