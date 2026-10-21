/**
 * A workspace's approval rules (#1216; IPC.md, "Approval rules"): the rules
 * of this workspace and the person's own (every workspace), each with its
 * decision, pattern, justification and examples; add, edit and delete; what
 * the repository proposes in `.elitea/rules.json`, listed rule by rule
 * (imported only on a click); and "Test a command", which shows the layer
 * and rule that would decide it. Every save is validated by the host first:
 * a rule whose examples disagree with it is refused with the rule and
 * example named, and nothing is written. A save names the file version it
 * was made from; when the file changed since (another window, an "always
 * allow"), nothing is written and the panel reloads and says so.
 */
import { Alert, Box, Button, CircularProgress, MenuItem, Stack, TextField, Typography } from '@mui/material';
import { useCallback, useEffect, useState } from 'react';

import { ruleSourceLabel } from '@/shared/desktop/ruleSource';
import type { PrefixRule, RuleDecision, RuleExplanation, RuleFile, RuleLoadError, RuleScope, RuleScopeState, RulesIpc, RulesView } from '@/shared/desktop/rulesIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { formatExample, formatPattern, parseExamples, parsePattern, PatternSyntaxError } from '../model/patternText';
import { ProposalReview } from './ProposalReview';
import { decisionLabel, LoadErrorAlert, RuleSummary } from './ruleParts';

export interface RulesPanelProps {
  ipc: RulesIpc;
  workspaceId: string;
}

const DECISIONS: readonly RuleDecision[] = ['allow', 'prompt', 'forbidden'];

interface Draft {
  pattern: string;
  decision: RuleDecision;
  justification: string;
  match: string;
  notMatch: string;
}

const EMPTY_DRAFT: Draft = { pattern: '', decision: 'allow', justification: '', match: '', notMatch: '' };

const draftOf = (rule: PrefixRule): Draft => ({
  pattern: formatPattern(rule.pattern),
  decision: rule.decision ?? 'allow',
  justification: rule.justification ?? '',
  match: (rule.match ?? []).map(formatExample).join('\n'),
  notMatch: (rule.not_match ?? []).map(formatExample).join('\n'),
});

/** The draft as a rule; the fields a draft cannot edit (sandbox, network) are kept from `base`. */
const ruleOf = (draft: Draft, base: PrefixRule | undefined): PrefixRule => {
  const rule: PrefixRule = { ...base, pattern: parsePattern(draft.pattern), decision: draft.decision };
  const justification = draft.justification.trim();
  if (justification === '') delete rule.justification;
  else rule.justification = justification;
  const match = parseExamples(draft.match);
  const notMatch = parseExamples(draft.notMatch);
  if (match.length > 0) rule.match = match;
  else delete rule.match;
  if (notMatch.length > 0) rule.not_match = notMatch;
  else delete rule.not_match;
  return rule;
};

/** Why the pattern's text does not parse, or `null`. */
function patternError(text: string): string | null {
  try {
    parsePattern(text);
    return null;
  } catch (error) {
    if (error instanceof PatternSyntaxError) return error.message;
    throw error;
  }
}

function RuleForm({ initial, busy, error, onSave, onCancel }: { initial: Draft; busy: boolean; error: RuleLoadError | null; onSave: (draft: Draft) => void; onCancel: () => void }): React.JSX.Element {
  const [draft, setDraft] = useState(initial);
  const field = (key: keyof Draft) => (event: React.ChangeEvent<HTMLInputElement>) => setDraft((current) => ({ ...current, [key]: event.target.value }));
  const syntax = patternError(draft.pattern);
  return (
    <Stack spacing={1.5} component="form" data-testid="rule-form" onSubmit={(event) => (event.preventDefault(), syntax === null && onSave(draft))}>
      <TextField
        size="small"
        label={t('workspace.rules.pattern', 'Command pattern')}
        error={syntax !== null}
        helperText={syntax === null ? t('workspace.rules.patternHelp', "Words in order, e.g. cargo [test|check]; * is any one word; quote a word with spaces, | or * ('My Tools/run', '*'). The command may go on after them.") : t('workspace.rules.patternInvalid', 'The pattern does not parse: {{message}}', { message: syntax })}
        value={draft.pattern}
        onChange={field('pattern')}
        required
      />
      <TextField size="small" select label={t('workspace.rules.decision', 'Decision')} value={draft.decision} onChange={field('decision')}>
        {DECISIONS.map((decision) => (
          <MenuItem key={decision} value={decision}>
            {decisionLabel(decision)}
          </MenuItem>
        ))}
      </TextField>
      <TextField size="small" label={t('workspace.rules.justification', 'Justification')} helperText={t('workspace.rules.justificationHelp', 'Shown to you and to the agent; for Forbidden, name what to do instead.')} value={draft.justification} onChange={field('justification')} />
      <TextField size="small" multiline minRows={2} label={t('workspace.rules.match', 'Examples it must match (one per line)')} value={draft.match} onChange={field('match')} />
      <TextField size="small" multiline minRows={2} label={t('workspace.rules.notMatch', 'Examples it must not match (one per line)')} value={draft.notMatch} onChange={field('notMatch')} />
      {error !== null && <LoadErrorAlert error={error} />}
      <Stack direction="row" spacing={1}>
        <Button type="submit" variant="contained" disabled={busy || draft.pattern.trim() === '' || syntax !== null}>
          {t('workspace.rules.save', 'Save rule')}
        </Button>
        <Button onClick={onCancel} disabled={busy}>
          {t('workspace.rules.cancel', 'Cancel')}
        </Button>
      </Stack>
    </Stack>
  );
}

/** Which rule of which scope is being edited (`index` undefined: a new one). */
interface Editing {
  scope: RuleScope;
  index: number | undefined;
}

function Explain({ ipc, workspaceId }: RulesPanelProps): React.JSX.Element {
  const [command, setCommand] = useState('');
  const [answer, setAnswer] = useState<RuleExplanation | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const test = async (): Promise<void> => {
    setFailure(null);
    try {
      setAnswer(await ipc.explain(workspaceId, command));
    } catch (error) {
      setAnswer(null);
      setFailure(toWorkspaceIpcError(error).message);
    }
  };
  const verdict = (value: RuleExplanation['verdict']): string => {
    if (value === 'deny') return t('workspace.rules.verdictDeny', 'Refused');
    if (value === 'ask') return t('workspace.rules.verdictAsk', 'Asked');
    return t('workspace.rules.verdictAllow', 'Allowed');
  };
  return (
    <Stack spacing={1} component="section" aria-label={t('workspace.rules.testTitle', 'Test a command')}>
      <Typography variant="labelMedium">{t('workspace.rules.testTitle', 'Test a command')}</Typography>
      <Stack direction="row" spacing={1} component="form" onSubmit={(event) => (event.preventDefault(), void test())}>
        <TextField size="small" fullWidth label={t('workspace.rules.testCommand', 'Command')} value={command} onChange={(event) => setCommand(event.target.value)} />
        <Button type="submit" variant="outlined" disabled={command.trim() === ''}>
          {t('workspace.rules.test', 'Test')}
        </Button>
      </Stack>
      {failure !== null && <Alert severity="error">{failure}</Alert>}
      {answer !== null && (
        <Box data-testid="rules-explain-result">
          <Typography variant="bodyMedium">
            {t('workspace.rules.explained', '{{verdict}} by {{source}}', {
              verdict: verdict(answer.verdict),
              source: ruleSourceLabel(answer.source, answer.rule === null ? null : `${answer.rule.scope}#${answer.rule.index}`) ?? answer.source,
            })}
            {answer.rule !== null && ` — ${answer.rule.pattern}`}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {answer.justification ?? answer.reason}
          </Typography>
        </Box>
      )}
    </Stack>
  );
}

interface ScopeSectionProps {
  state: RuleScopeState;
  /** The rule of this scope being edited, if any. */
  editing: Editing | null;
  busy: boolean;
  formError: RuleLoadError | null;
  /** `undefined`: add a new rule. */
  onEdit: (index: number | undefined) => void;
  onDelete: (index: number) => void;
  onSave: (draft: Draft) => void;
  onCancel: () => void;
}

function RuleRow({ scope, rule, index, busy, onEdit, onDelete }: { scope: RuleScope; rule: PrefixRule; index: number; busy: boolean; onEdit: () => void; onDelete: () => void }): React.JSX.Element {
  return (
    <Box component="li" data-testid={`rule-${scope}-${index + 1}`} sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-start' }}>
      <RuleSummary rule={rule} />
      <Button size="small" onClick={onEdit} disabled={busy}>
        {t('workspace.rules.edit', 'Edit')}
      </Button>
      <Button size="small" color="error" onClick={onDelete} disabled={busy}>
        {t('workspace.rules.delete', 'Delete')}
      </Button>
    </Box>
  );
}

/** The rule being edited or added, else "Add rule" (a refused file cannot be edited here). */
function ScopeFooter({ state, editing, busy, formError, onEdit, onSave, onCancel }: ScopeSectionProps): React.JSX.Element | null {
  if (editing !== null) {
    const rule = editing.index === undefined ? undefined : state.file?.rules[editing.index];
    return <RuleForm key={`${state.scope}-${editing.index ?? 'new'}`} initial={rule === undefined ? EMPTY_DRAFT : draftOf(rule)} busy={busy} error={formError} onSave={onSave} onCancel={onCancel} />;
  }
  if (state.file === null) return null;
  return (
    <Box>
      <Button size="small" variant="outlined" onClick={() => onEdit(undefined)} disabled={busy}>
        {t('workspace.rules.add', 'Add rule')}
      </Button>
    </Box>
  );
}

function ScopeSection({ state, editing, busy, formError, onEdit, onDelete, onSave, onCancel }: ScopeSectionProps): React.JSX.Element {
  const title = state.scope === 'workspace' ? t('workspace.rules.workspaceTitle', 'This workspace') : t('workspace.rules.userTitle', 'Every workspace (your rules)');
  const rules = state.file?.rules ?? [];
  return (
    <Stack spacing={1.5} component="section" data-testid={`rules-scope-${state.scope}`} aria-label={title}>
      <Typography variant="labelMedium">{title}</Typography>
      <Typography variant="bodySmall" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
        {state.path}
      </Typography>
      {state.error !== null && <LoadErrorAlert error={state.error} />}
      {state.file !== null && rules.length === 0 && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('workspace.rules.none', 'No rules yet: the defaults apply.')}
        </Typography>
      )}
      {rules.length > 0 && (
        <Stack component="ol" spacing={1} sx={{ listStyle: 'none', p: 0, m: 0 }}>
          {rules.map((rule, index) => (
            <RuleRow key={`${index}-${formatPattern(rule.pattern)}`} scope={state.scope} rule={rule} index={index} busy={busy} onEdit={() => onEdit(index)} onDelete={() => onDelete(index)} />
          ))}
        </Stack>
      )}
      <ScopeFooter state={state} editing={editing} busy={busy} formError={formError} onEdit={onEdit} onDelete={onDelete} onSave={onSave} onCancel={onCancel} />
    </Stack>
  );
}

export function RulesPanel({ ipc, workspaceId }: RulesPanelProps): React.JSX.Element {
  const [view, setView] = useState<RulesView | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [editing, setEditing] = useState<Editing | null>(null);
  const [formError, setFormError] = useState<RuleLoadError | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setView(await ipc.get(workspaceId));
      setFailure(null);
    } catch (error) {
      setFailure(toWorkspaceIpcError(error).message);
    }
  }, [ipc, workspaceId]);

  useEffect(() => {
    setView(null);
    setEditing(null);
    void load();
  }, [load]);

  const [notice, setNotice] = useState<string | null>(null);

  const store = async (scope: RuleScope, file: RuleFile): Promise<boolean> => {
    setBusy(true);
    setNotice(null);
    try {
      const version = view?.scopes.find((state) => state.scope === scope)?.version ?? 'absent';
      const outcome = await ipc.put(workspaceId, scope, file, version);
      if (outcome.conflict) {
        // Saved over nothing: the file changed since it was read. Show it
        // as it is now; the person makes the change again.
        setEditing(null);
        setFormError(null);
        setNotice(t('workspace.rules.conflict', 'These rules changed somewhere else (another window, or an “always allow”) since they were shown, so nothing was saved. They have been reloaded: make your change again.'));
        await load();
        return false;
      }
      if (!outcome.saved) {
        setFormError(outcome.error);
        return false;
      }
      setFormError(null);
      await load();
      return true;
    } catch (error) {
      setFailure(toWorkspaceIpcError(error).message);
      return false;
    } finally {
      setBusy(false);
    }
  };

  const fileOf = (scope: RuleScope): RuleFile => view?.scopes.find((state) => state.scope === scope)?.file ?? { version: 1, rules: [] };

  const save = async (draft: Draft): Promise<void> => {
    if (editing === null) return;
    const file = fileOf(editing.scope);
    const rules = [...file.rules];
    const rule = ruleOf(draft, editing.index === undefined ? undefined : rules[editing.index]);
    if (editing.index === undefined) rules.push(rule);
    else rules[editing.index] = rule;
    if (await store(editing.scope, { ...file, rules })) setEditing(null);
  };

  const remove = async (scope: RuleScope, index: number): Promise<void> => {
    const file = fileOf(scope);
    await store(scope, { ...file, rules: file.rules.filter((_, position) => position !== index) });
  };

  const importProposed = async (digest: string): Promise<void> => {
    setBusy(true);
    setNotice(null);
    try {
      const outcome = await ipc.importProposed(workspaceId, digest);
      if (outcome.conflict) setNotice(t('workspace.rules.proposedConflict', 'The repository’s proposal changed since it was shown, so nothing was imported. Review it again below.'));
      else if (!outcome.saved && outcome.error !== null) setFailure(outcome.error.message);
      await load();
    } catch (error) {
      setFailure(toWorkspaceIpcError(error).message);
    } finally {
      setBusy(false);
    }
  };

  if (view === null) {
    return failure === null ? <CircularProgress size={24} aria-label={t('workspace.rules.loading', 'Loading rules')} /> : <Alert severity="error">{failure}</Alert>;
  }

  return (
    <Stack spacing={3}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'workspace.rules.help',
          'Rules decide which commands run without asking, which are asked, and which are refused. An Allow rule covers one plain command, never a command line with pipes or lists. Forbidden rules are advisory: they catch the commands Elitea can see, not what a script starts; the sandbox is what bounds a command. Your organisation’s policy always comes first.',
        )}
      </Typography>
      {failure !== null && <Alert severity="error">{failure}</Alert>}
      {notice !== null && (
        <Alert severity="warning" data-testid="rules-notice">
          {notice}
        </Alert>
      )}
      {view.proposed !== null && <ProposalReview proposal={view.proposed} busy={busy} onImport={(digest) => void importProposed(digest)} />}
      {view.scopes.map((state) => (
        <ScopeSection
          key={state.scope}
          state={state}
          editing={editing?.scope === state.scope ? editing : null}
          busy={busy}
          formError={formError}
          onEdit={(index) => (setFormError(null), setEditing({ scope: state.scope, index }))}
          onDelete={(index) => void remove(state.scope, index)}
          onSave={(draft) => void save(draft)}
          onCancel={() => setEditing(null)}
        />
      ))}
      <Explain ipc={ipc} workspaceId={workspaceId} />
    </Stack>
  );
}
