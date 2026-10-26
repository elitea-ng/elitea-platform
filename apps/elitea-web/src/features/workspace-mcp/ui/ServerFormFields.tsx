/**
 * The add/edit form of one MCP server (#1230): its fields, each checked
 * against the host's schema as it is typed (`checkForm`), and Save only
 * once nothing is wrong. Saving starts nothing: the next turn that may
 * offer the server's tools starts it, under the sandbox.
 */
import { Box, Button, FormControlLabel, MenuItem, Stack, TextField } from '@mui/material';
import { useState } from 'react';

import type { McpScope, McpServerConfig } from '@/shared/desktop/mcpIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { checkForm, type FormProblem, type ServerForm } from '../model/serverForm';

/** A field's problem, in words. */
function problemText(problem: FormProblem | undefined): string | undefined {
  if (problem === undefined) return undefined;
  switch (problem.code) {
    case 'name':
      return t('workspace.mcp.form.nameInvalid', 'Letters, digits, - and _ only, at most 32.');
    case 'command':
      return t('workspace.mcp.form.commandInvalid', 'The full path of the program, starting with /.');
    case 'argsCount':
      return t('workspace.mcp.form.argsCount', 'At most 64 arguments.');
    case 'argLength':
      return t('workspace.mcp.form.argLength', 'An argument is longer than 4096 characters.');
    case 'envCount':
      return t('workspace.mcp.form.envCount', 'At most 64 variables.');
    case 'envKey':
      return t('workspace.mcp.form.envKey', '“{{key}}” is not a variable name: A–Z, 0–9 and _, not starting with a digit.', { key: problem.key });
    case 'envValue':
      return t('workspace.mcp.form.envValue', 'The value of {{key}} is longer than 4096 characters.', { key: problem.key });
  }
}

export interface ServerFormFieldsProps {
  initial: ServerForm;
  /** Editing a saved server: its name and file stay. */
  editing: boolean;
  busy: boolean;
  onSave: (scope: McpScope, name: string, server: McpServerConfig) => void;
  onCancel: () => void;
}

export function ServerFormFields({ initial, editing, busy, onSave, onCancel }: ServerFormFieldsProps): React.JSX.Element {
  const [form, setForm] = useState(initial);
  const { server, errors } = checkForm(form);
  const text = (key: 'name' | 'command' | 'args' | 'env' | 'allowedTools' | 'readOnlyTools') => (event: React.ChangeEvent<HTMLInputElement>) =>
    setForm((current) => ({ ...current, [key]: event.target.value }));
  const flag = (key: 'network' | 'enabled' | 'trustReadOnlyHints') => (event: React.ChangeEvent<HTMLInputElement>) =>
    setForm((current) => ({ ...current, [key]: event.target.checked }));
  const invalid = Object.keys(errors).length > 0;
  const submit = (event: React.SyntheticEvent): void => {
    event.preventDefault();
    if (!invalid) onSave(form.scope, form.name, server);
  };
  return (
    <Stack spacing={1.5} component="form" data-testid="mcp-server-form" aria-label={t('workspace.mcp.form.title', 'MCP server')} onSubmit={submit}>
      <Stack direction="row" spacing={1.5}>
        <TextField size="small" label={t('workspace.mcp.form.name', 'Name')} value={form.name} onChange={text('name')} disabled={editing} error={errors.name !== undefined} helperText={problemText(errors.name)} />
        <TextField
          select
          size="small"
          label={t('workspace.mcp.form.scope', 'Used in')}
          value={form.scope}
          disabled={editing}
          onChange={(event) => setForm((current) => ({ ...current, scope: event.target.value as McpScope }))}
          sx={{ minWidth: 200 }}
        >
          <MenuItem value="user">{t('workspace.mcp.scopeUser', 'Every workspace')}</MenuItem>
          <MenuItem value="workspace">{t('workspace.mcp.scopeWorkspace', 'This workspace')}</MenuItem>
        </TextField>
      </Stack>
      <TextField size="small" label={t('workspace.mcp.form.command', 'Program')} value={form.command} onChange={text('command')} error={errors.command !== undefined} helperText={problemText(errors.command)} />
      <TextField size="small" multiline minRows={2} label={t('workspace.mcp.form.args', 'Arguments, one per line')} value={form.args} onChange={text('args')} error={errors.args !== undefined} helperText={problemText(errors.args)} />
      <TextField
        size="small"
        multiline
        minRows={2}
        label={t('workspace.mcp.form.env', 'Environment, one NAME=value per line')}
        value={form.env}
        onChange={text('env')}
        error={errors.env !== undefined}
        helperText={problemText(errors.env) ?? t('workspace.mcp.form.envHelp', 'The server gets these and the basic ones (PATH, HOME, LANG), never the app’s own.')}
      />
      <TextField
        select
        size="small"
        label={t('workspace.mcp.form.cwd', 'Runs in')}
        value={form.cwd}
        onChange={(event) => setForm((current) => ({ ...current, cwd: event.target.value as ServerForm['cwd'] }))}
        sx={{ maxWidth: 320 }}
      >
        <MenuItem value="workspace">{t('workspace.mcp.form.cwdWorkspace', 'The workspace folder')}</MenuItem>
        <MenuItem value="none">{t('workspace.mcp.form.cwdNone', 'A private temporary folder')}</MenuItem>
      </TextField>
      <TextField size="small" label={t('workspace.mcp.form.allowedTools', 'Only these tools (comma-separated; empty: all)')} value={form.allowedTools} onChange={text('allowedTools')} />
      <TextField
        size="small"
        label={t('workspace.mcp.form.readOnlyTools', 'Read-only tools (comma-separated)')}
        value={form.readOnlyTools}
        onChange={text('readOnlyTools')}
        helperText={t('workspace.mcp.form.readOnlyHelp', 'Read-only tools run without asking, also in plan mode; the others are asked the first time.')}
      />
      <Box>
        <FormControlLabel control={<BaseCheckbox checked={form.trustReadOnlyHints} onChange={flag('trustReadOnlyHints')} />} label={t('workspace.mcp.form.trustHints', 'Trust the server’s own read-only hints')} />
        <FormControlLabel control={<BaseCheckbox checked={form.network} onChange={flag('network')} />} label={t('workspace.mcp.form.network', 'Network access (asked when it starts)')} />
        <FormControlLabel control={<BaseCheckbox checked={form.enabled} onChange={flag('enabled')} />} label={t('workspace.mcp.form.enabled', 'Enabled')} />
      </Box>
      <Stack direction="row" spacing={1}>
        <Button type="submit" size="small" variant="contained" disabled={busy || invalid}>
          {t('workspace.mcp.form.save', 'Save')}
        </Button>
        <Button size="small" onClick={onCancel} disabled={busy}>
          {t('workspace.mcp.form.cancel', 'Cancel')}
        </Button>
      </Stack>
    </Stack>
  );
}
