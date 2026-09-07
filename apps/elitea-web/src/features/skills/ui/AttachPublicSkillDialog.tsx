/**
 * "Use this skill" — fork a published skill into this project and attach it to
 * one of the caller's agents.
 *
 * Ported, much reduced, from the reference's `AttachToAgentDialog.jsx`. What is
 * kept is the part that carries meaning: an attach targets an agent VERSION,
 * not an agent, so the version is chosen explicitly rather than guessed. The
 * reference guesses when the agent has exactly one; this asks either way,
 * because "attached to your agent" is ambiguous the moment an agent has two
 * versions and the wrong one silently received the skill.
 *
 * The per-agent outcome is shown from the response's `results` list, not from
 * the HTTP status: the route answers 200 even when an individual attachment
 * failed, so a dialog that read the status alone would report success for an
 * attach that attached nothing.
 *
 * Issue #625 item 3 adds the piece the port dropped: `agents_with_skill`
 * (`internal/api/router.go` skill-publish routes) told the reference which
 * agent versions already carry this skill, through the fork lineage, so the
 * dialog could mark them rather than let a second attach hit the server's 409.
 * That endpoint had a generated client and zero call sites before this; the
 * version picker below is the one.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Typography from '@mui/material/Typography';

import { useGetApplication, useListApplications } from '@/shared/api/generated/applications/applications';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { unwrapList } from '@/shared/api/unwrap';

import { publishErrorMessage } from '../api/skillPublishApi';
import { useAgentsWithSkill, useAttachPublicSkill, type AttachSkillArgs } from '../model/usePublicSkills';
import type { AgentWithSkill, AttachOutcome, PublicSkillSummary } from '../model/publishTypes';

interface AgentRow {
  readonly id?: string | number;
  readonly name?: string;
}

interface AgentVersionRow {
  readonly id?: string | number;
  readonly name?: string;
}

export interface AttachPublicSkillDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly skill: PublicSkillSummary | undefined;
  readonly versionId: number | undefined;
  readonly onClose: () => void;
}

/**
 * One labelled `Select` over `{value, label}` rows. Both pickers in this dialog
 * are the same control over different rows, and inlining them twice is what
 * pushed the component past the §3.5 complexity budget.
 */
function RowSelect({
  id,
  label,
  value,
  options,
  disabled = false,
  onChange,
}: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly options: readonly SelectOption[];
  readonly disabled?: boolean;
  readonly onChange: (value: string) => void;
}): ReactNode {
  return (
    <FormControl
      size="small"
      disabled={disabled}
    >
      <InputLabel id={id}>{label}</InputLabel>
      <Select
        labelId={id}
        label={label}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      >
        {options.map((option) => (
          <MenuItem
            key={option.value}
            value={option.value}
            disabled={option.disabled === true}
          >
            {option.label}
          </MenuItem>
        ))}
      </Select>
    </FormControl>
  );
}

interface SelectOption {
  readonly value: string;
  readonly label: string;
  /** Already carries the skill (issue #625 item 3) — shown, not hidden, and not selectable. */
  readonly disabled?: boolean;
}

/**
 * The two option lists this dialog picks from: the project's agents, and the
 * versions of the one that is selected.
 *
 * A hook rather than two inline queries so the component stays inside the §3.5
 * complexity budget — every `??` and `?.` on a response counts, and there are
 * six of them here.
 */
function useAttachTargets(
  projectId: string | undefined,
  open: boolean,
  agentId: string,
  attachedVersionIds: ReadonlySet<number>,
): { readonly agents: readonly SelectOption[]; readonly versions: readonly SelectOption[] } {
  const agents = useListApplications(
    projectId ?? '',
    { agents_type: 'classic' },
    { query: { enabled: open && projectId !== undefined } },
  );
  // `getApplication` takes a NUMERIC application id while the list carries it
  // as a string; the conversion happens here rather than in the dialog's state
  // so the Select's value stays the string the row supplied.
  const detail = useGetApplication(projectId ?? '', Number(agentId), {
    query: { enabled: open && projectId !== undefined && agentId !== '' },
  });

  const agentRows = unwrapList<AgentRow>(agents.data?.data, 'listApplications(attach-skill)');
  const versionRows: readonly AgentVersionRow[] =
    (detail.data?.data as { readonly versions?: readonly AgentVersionRow[] } | undefined)?.versions ?? [];

  return {
    agents: agentRows.map((agent) => ({ value: String(agent.id ?? ''), label: agent.name ?? '' })),
    versions: versionRows.map((version) => markIfAlreadyAttached(version, attachedVersionIds)),
  };
}

/**
 * One version option, marked "already attached" (disabled, suffixed label)
 * when `agents_with_skill` named its id — the write route answers a second
 * attach with 409, so this dialog never offers one.
 */
function markIfAlreadyAttached(version: AgentVersionRow, attachedVersionIds: ReadonlySet<number>): SelectOption {
  const value = String(version.id);
  const label = version.name ?? '';
  if (!attachedVersionIds.has(Number(value))) return { value, label };
  return {
    value,
    label: `${label} ${t('skills.attach.alreadyAttachedSuffix', '(already attached)')}`,
    disabled: true,
  };
}

/** The `entity_version_id`s `agents_with_skill` named, as a lookup set. */
export function alreadyAttachedVersionIds(rows: readonly AgentWithSkill[]): ReadonlySet<number> {
  const ids = new Set<number>();
  for (const row of rows) {
    if (row.entity_version_id !== undefined) ids.add(row.entity_version_id);
  }
  return ids;
}

/** The distinct agent names `agents_with_skill` named, in first-seen order. */
export function alreadyAttachedAgentNames(rows: readonly AgentWithSkill[]): readonly string[] {
  const seen = new Set<string>();
  for (const row of rows) {
    if (row.name !== undefined && row.name !== '') seen.add(row.name);
  }
  return [...seen];
}

/**
 * The attach request, or `undefined` when the dialog is not ready to send one.
 *
 * All three ids have to be present and the agent version has to be a real
 * number: `Number('')` is 0, and an attach aimed at agent version 0 is a
 * request the server can only refuse.
 */
export function attachRequestOf(
  publicSkillId: number | undefined,
  publicVersionId: number | undefined,
  agentVersionId: string,
): AttachSkillArgs | undefined {
  const agentVersion = Number(agentVersionId);
  if (!publicSkillId || publicVersionId === undefined || agentVersionId === '' || !agentVersion) {
    return undefined;
  }
  return { publicSkillId, publicVersionId, agentVersionIds: [agentVersion] };
}

/**
 * Reads the per-agent `results` list rather than the HTTP status.
 *
 * Split out of the component to keep it inside the §3.5 complexity budget, and
 * exported so the "200 that attached nothing" case can be asserted directly.
 */
export function reportOutcome(
  results: readonly AttachOutcome[],
  setMessage: (message: string) => void,
  onDone: () => void,
): void {
  const failed = results.find((result) => result.ok !== true);
  if (failed) {
    setMessage(
      failed.error ?? t('skills.attach.partial', 'The skill could not be attached to that agent version.'),
    );
    return;
  }
  onDone();
}

export function AttachPublicSkillDialog({
  open,
  projectId,
  skill,
  versionId,
  onClose,
}: AttachPublicSkillDialogProps): ReactNode {
  const [agentId, setAgentId] = useState('');
  const [agentVersionId, setAgentVersionId] = useState('');
  const [message, setMessage] = useState<string>();

  /**
   * The catalog keeps ONE of these mounted and toggles `open`, so without this
   * every field survives a close: the next skill opened would show the previous
   * one's error message, with the previous agent and version still selected and
   * Attach already enabled — one click from attaching this skill to a version
   * nobody chose for it.
   */
  useEffect(() => {
    setAgentId('');
    setAgentVersionId('');
    setMessage(undefined);
  }, [open, skill?.id]);

  const attach = useAttachPublicSkill(projectId);
  const agentsWithSkill = useAgentsWithSkill(projectId, skill?.id, open);
  const attachedRows = agentsWithSkill.data ?? [];
  const attachedVersionIds = alreadyAttachedVersionIds(attachedRows);
  const attachedAgentNames = alreadyAttachedAgentNames(attachedRows);
  const targets = useAttachTargets(projectId, open, agentId, attachedVersionIds);

  const confirm = (): void => {
    setMessage(undefined);
    const request = attachRequestOf(skill?.id, versionId, agentVersionId);
    if (!request) return;
    void attach
      .mutateAsync(request)
      .then((results) => reportOutcome(results, setMessage, onClose))
      .catch((error: unknown) => {
        setMessage(publishErrorMessage(error, t('skills.attach.failed', 'Failed to attach the skill.')));
      });
  };

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={confirm}
      title={t('skills.attach.title', 'Use this skill in an agent')}
      data-testid="attach-public-skill-modal"
      actions={{
        confirmText: t('skills.attach.confirm', 'Attach'),
        confirming: attach.isPending || agentVersionId === '',
      }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 420 }}>
          <Typography variant="bodySmall">
            {t(
              'skills.attach.explain',
              'The skill is copied into this project once, then attached to the agent version you choose.',
            )}
          </Typography>
          {attachedAgentNames.length > 0 && (
            <Alert
              severity="info"
              data-testid="attach-public-skill-already-attached"
            >
              {t('skills.attach.alreadyAttachedBanner', 'Already attached to: {{names}}', {
                names: attachedAgentNames.join(', '),
              })}
            </Alert>
          )}
          <RowSelect
            id="attach-skill-agent"
            label={t('skills.attach.agent', 'Agent')}
            value={agentId}
            options={targets.agents}
            onChange={(next) => {
              setAgentId(next);
              setAgentVersionId('');
            }}
          />
          <RowSelect
            id="attach-skill-version"
            label={t('skills.attach.version', 'Agent version')}
            value={agentVersionId}
            disabled={agentId === ''}
            options={targets.versions}
            onChange={setAgentVersionId}
          />
          {attach.isPending && <LinearProgress />}
          {message !== undefined && (
            <Alert
              severity="error"
              data-testid="attach-public-skill-error"
            >
              {message}
            </Alert>
          )}
        </Box>
      }
    />
  );
}
