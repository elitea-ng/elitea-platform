/**
 * The card a repository's `.elitea/hooks.json` (or the hooks of its
 * `.claude/settings.json`) becomes in the session (#1237): the hooks it
 * proposes, each with its event, command, sandbox and network switch, and
 * the person's answer. Nothing runs from the file: the hooks ticked are
 * copied into this workspace's settings for this version of the file, and
 * run from the next turn; "Not now" answers it too (a later change of the
 * file is a new proposal). Refreshed when the host sends one
 * (`hooks://event`).
 */
import { Alert, Box, Button, FormControlLabel, Stack, Typography } from '@mui/material';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useState } from 'react';

import { tauriHooksIpc, type HooksIpc, type HooksProposal, type ProposedHook } from '@/shared/desktop/hooksIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { hooksListKey } from './hooksQuery';

function Proposed({ hook, checked, onToggle }: { hook: ProposedHook; checked: boolean; onToggle: () => void }): React.JSX.Element {
  const switches = [hook.event, hook.sandbox ?? 'read-only', hook.network === true ? t('workspace.hooks.network', 'Network') : null].filter((x) => x !== null).join(' · ');
  return (
    <Box component="li" data-testid={`hook-proposed-${hook.id}`} sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <FormControlLabel control={<BaseCheckbox checked={checked} onChange={onToggle} disabled={hook.problem !== undefined} />} label={hook.id} />
      <Typography variant="bodySmall" color="text.secondary" sx={{ pl: 4 }}>
        {switches}
      </Typography>
      <Typography variant="bodySmall" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere', pl: 4 }}>
        {hook.command.join(' ')}
      </Typography>
      {hook.problem !== undefined && (
        <Typography variant="bodySmall" color="error" sx={{ pl: 4 }}>
          {t('workspace.hooks.proposalProblem', 'Cannot be trusted: {{problem}}', { problem: hook.problem })}
        </Typography>
      )}
    </Box>
  );
}

function ProposalCard({ proposal, busy, onAnswer }: { proposal: HooksProposal; busy: boolean; onAnswer: (accept: string[]) => void }): React.JSX.Element {
  const [picked, setPicked] = useState<string[]>([]);
  const toggle = (id: string): void => setPicked((current) => (current.includes(id) ? current.filter((x) => x !== id) : [...current, id]));
  return (
    <Alert severity="info" data-testid="hooks-proposal" icon={false}>
      <Stack spacing={1}>
        <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
          {t('workspace.hooks.proposalTitle', 'This repository proposes {{count}} hooks ({{file}})', { count: proposal.hooks.length, file: proposal.file })}
        </Typography>
        <Typography variant="bodySmall">
          {t(
            'workspace.hooks.proposalHelp',
            'Nothing runs from the file. The hooks you trust are copied into this workspace’s settings and run on this computer from the next turn, each under the sandbox shown. If the file changes, you are asked again.',
          )}
        </Typography>
        <Stack component="ul" spacing={0.5} sx={{ listStyle: 'none', p: 0, m: 0 }}>
          {proposal.hooks.map((hook) => (
            <Proposed key={hook.id} hook={hook} checked={picked.includes(hook.id)} onToggle={() => toggle(hook.id)} />
          ))}
        </Stack>
        <Stack direction="row" spacing={1}>
          <Button size="small" variant="contained" disabled={busy || picked.length === 0} onClick={() => onAnswer(picked)}>
            {t('workspace.hooks.proposalTrust', 'Trust the selected hooks')}
          </Button>
          <Button size="small" disabled={busy} onClick={() => onAnswer([])}>
            {t('workspace.hooks.proposalDecline', 'Not now')}
          </Button>
        </Stack>
      </Stack>
    </Alert>
  );
}

export interface HooksProposalsProps {
  workspaceId: string;
  /** Tests pass a fake; the app uses the host's. */
  ipc?: HooksIpc;
}

export function HooksProposals({ workspaceId, ipc: given }: HooksProposalsProps): React.JSX.Element | null {
  const ipc = useMemo(() => given ?? tauriHooksIpc(), [given]);
  const queryClient = useQueryClient();
  const key = hooksListKey(workspaceId);
  const list = useQuery({ queryKey: key, queryFn: () => ipc?.list(workspaceId) ?? Promise.reject(new Error('no host')), enabled: ipc !== undefined });
  const [failure, setFailure] = useState<string | null>(null);
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let live = true;
    let unlisten: () => void = () => undefined;
    void ipc
      .onProposal((event) => {
        if (event.workspace_id === workspaceId) void queryClient.invalidateQueries({ queryKey: hooksListKey(workspaceId) });
      })
      .then(
        (off) => {
          if (!live) off();
          else unlisten = off;
        },
        () => undefined,
      );
    return () => {
      live = false;
      unlisten();
    };
  }, [ipc, queryClient, workspaceId]);
  const answer = useMutation({
    mutationFn: ({ digest, accept }: { digest: string; accept: string[] }) => ipc?.answerProposal(workspaceId, digest, accept) ?? Promise.reject(new Error('no host')),
    onSuccess: () => {
      setFailure(null);
      return queryClient.invalidateQueries({ queryKey: key });
    },
    onError: (error) => setFailure(toWorkspaceIpcError(error).message),
  });
  const proposals = list.data?.proposals ?? [];
  if (proposals.length === 0) return null;
  return (
    <Stack spacing={1}>
      {failure !== null && <Alert severity="error">{failure}</Alert>}
      {proposals.map((proposal) => (
        <ProposalCard key={proposal.file_sha256} proposal={proposal} busy={answer.isPending} onAnswer={(accept) => answer.mutate({ digest: proposal.file_sha256, accept })} />
      ))}
    </Stack>
  );
}
