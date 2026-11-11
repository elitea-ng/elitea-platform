/**
 * "Copy to project" for a personal agent (ADR-0050 decision 1): an
 * independent agent in a team project, optionally with some of its memory.
 *
 * The copy is the editor's Fork (the fork document read from the personal
 * project and written to the target), so the new agent is an agent of its
 * own: editing either never changes the other. The memory a person picks is
 * then copied by the copy-memory route, which re-checks that the source is
 * the caller's personal project and that the caller may read that memory,
 * and copies only active entries about nobody. An entry about a person never
 * leaves the personal project, whatever is picked.
 *
 * `projects` is a prop for the reason `EntityLifecycleControls` gives: the
 * project list lives in a widget a feature may not import.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useMutation, useQuery } from '@tanstack/react-query';

import type { AgentMemoryEntry } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { copyMemoryInto, fetchCopyableMemory, forkAgentInto } from '../api/agentProposalsApi';
import { AgentMadeError } from '../lib/proposalErrors';

export interface CopyTargetProject {
  readonly id: string;
  readonly name: string;
}

export interface CopyToProjectButtonProps {
  /** The personal project the agent lives in. */
  readonly projectId: string;
  readonly applicationId: number;
  readonly agentName: string;
  /** Team projects the copy may go to (never the personal project itself). */
  readonly projects: readonly CopyTargetProject[];
  /** Called with the copy's project and id once it exists. */
  readonly onCopied?: ((targetProjectId: string, applicationId: number, memoryCopied: number) => void) | undefined;
}

interface CopyVariables {
  readonly targetProjectId: string;
  readonly entryIds: readonly number[];
}

/** An entry about a person stays in the personal project; the route would skip it anyway. */
export function copyableEntries(rows: readonly AgentMemoryEntry[]): readonly AgentMemoryEntry[] {
  return rows.filter((entry) => entry.subject_user_id === null && entry.status === 'active' && entry.kind !== 'cursor');
}

export function CopyToProjectButton({ projectId, applicationId, agentName, projects, onCopied }: CopyToProjectButtonProps): ReactNode {
  const [open, setOpen] = useState(false);
  if (projects.length === 0) return null;
  return (
    <>
      <Button variant="outlined" size="small" onClick={() => setOpen(true)} data-testid="agent-copy-to-project">
        {t('features.agentProposals.copy.button', 'Copy to project')}
      </Button>
      {open && (
        <CopyToProjectDialog
          projectId={projectId}
          applicationId={applicationId}
          agentName={agentName}
          projects={projects}
          onCopied={onCopied}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

function CopyToProjectDialog({
  projectId,
  applicationId,
  agentName,
  projects,
  onCopied,
  onClose,
}: CopyToProjectButtonProps & { readonly onClose: () => void }): ReactNode {
  const [target, setTarget] = useState(projects[0]?.id ?? '');
  const [picked, setPicked] = useState<ReadonlySet<number>>(new Set());
  const [error, setError] = useState<string | undefined>(undefined);
  // The copy an attempt forked before its memory copy failed, by target
  // project: a retry copies the memory into it instead of forking again.
  const [made, setMade] = useState<{ readonly target: string; readonly copyId: number } | undefined>(undefined);
  const memory = useQuery({
    queryKey: ['agent-copy-memory', projectId, applicationId],
    queryFn: () => fetchCopyableMemory(projectId, applicationId),
    retry: false,
  });
  const entries = copyableEntries(memory.data?.rows ?? []);
  const copy = useMutation({
    mutationFn: async ({ targetProjectId, entryIds }: CopyVariables) => {
      const copyId = made?.target === targetProjectId ? made.copyId : await forkAgentInto(projectId, applicationId, targetProjectId);
      if (copyId === undefined) throw new Error('fork answered no agent');
      try {
        const copied = entryIds.length === 0 ? 0 : await copyMemoryInto(targetProjectId, copyId, Number(projectId), applicationId, entryIds);
        return { copyId, copied };
      } catch (cause) {
        setMade({ target: targetProjectId, copyId });
        throw new AgentMadeError(copyId, cause);
      }
    },
  });

  const toggle = (id: number, checked: boolean): void =>
    setPicked((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });

  const submit = (): void => {
    if (target === '' || copy.isPending) return;
    setError(undefined);
    copy.mutate(
      { targetProjectId: target, entryIds: [...picked] },
      {
        onSuccess: ({ copyId, copied }) => {
          onCopied?.(target, copyId, copied);
          onClose();
        },
        onError: (cause) =>
          setError(
            cause instanceof AgentMadeError
              ? serverErrorMessage(
                  cause.cause,
                  t('features.agentProposals.copy.memoryFailed', 'The agent was copied, but its memory was not. Try again to copy the memory.'),
                )
              : serverErrorMessage(cause, t('features.agentProposals.copy.failed', 'The agent was not copied. Try again.')),
          ),
      },
    );
  };

  return (
    <BaseModal
      open
      data-testid="agent-copy-to-project-dialog"
      title={t('features.agentProposals.copy.title', 'Copy {{name}} to a project', { name: agentName })}
      onClose={onClose}
      onConfirm={submit}
      actions={{ confirmText: t('features.agentProposals.copy.confirm', 'Copy'), confirming: copy.isPending || target === '' }}
      content={
        <Box sx={formSx}>
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentProposals.copy.independent', 'The copy is an agent of its own. Changing one never changes the other.')}
          </Typography>
          <TextField
            select
            label={t('features.agentProposals.copy.project', 'Project')}
            value={target}
            onChange={(event) => setTarget(event.target.value)}
            slotProps={{ htmlInput: { 'data-testid': 'agent-copy-target' } }}
          >
            {projects.map((project) => (
              <MenuItem key={project.id} value={project.id}>
                {project.name}
              </MenuItem>
            ))}
          </TextField>
          <Typography variant="labelMedium">{t('features.agentProposals.copy.memory', 'Memory to copy')}</Typography>
          {entries.length === 0 ? (
            <Typography variant="bodySmall" color="text.secondary" data-testid="agent-copy-no-memory">
              {t('features.agentProposals.copy.noMemory', 'Nothing in this agent’s memory can be copied.')}
            </Typography>
          ) : (
            entries.map((entry) => (
              <FormControlLabel
                key={entry.id}
                control={
                  <BaseCheckbox checked={picked.has(entry.id)} onChange={(_event, checked) => toggle(entry.id, checked)} data-testid={`agent-copy-entry-${entry.id}`} />
                }
                label={entry.content}
              />
            ))
          )}
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentProposals.copy.private', 'Memory about a person stays in your personal project.')}
          </Typography>
          {error !== undefined && (
            <Typography role="alert" variant="bodySmall" color="error" data-testid="agent-copy-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', minWidth: '28rem' };
