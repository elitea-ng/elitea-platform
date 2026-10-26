/**
 * The card a repository's `.mcp.json` (or Cursor's, or VS Code's) becomes
 * in the session (#1230): the servers it proposes, each with its program,
 * arguments and the names of its variables (their values masked), and the
 * person's answer. Nothing runs from the file: the servers ticked are
 * copied into this workspace's settings, and start with the next turn
 * under the sandbox; "Not now" answers it too (a later change of the file
 * is a new proposal). Refreshed when the host sends one (`mcp://event`).
 */
import { Alert, Box, Button, FormControlLabel, Stack, Typography } from '@mui/material';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useState } from 'react';

import { tauriMcpIpc, type McpIpc, type McpProposal, type McpProposedServer } from '@/shared/desktop/mcpIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { mcpListKey } from './mcpQuery';

function ProposedServer({ server, checked, onToggle }: { server: McpProposedServer; checked: boolean; onToggle: () => void }): React.JSX.Element {
  const env = server.env_keys.map((key) => `${key}=${server.env_preview[key] ?? ''}`).join('  ');
  return (
    <Box component="li" data-testid={`mcp-proposed-${server.name}`} sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <FormControlLabel control={<BaseCheckbox checked={checked} onChange={onToggle} disabled={server.problem !== undefined} />} label={server.name} />
      <Typography variant="bodySmall" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere', pl: 4 }}>
        {[server.command, ...server.args].join(' ')}
      </Typography>
      {env !== '' && (
        <Typography variant="bodySmall" color="text.secondary" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere', pl: 4 }}>
          {env}
        </Typography>
      )}
      {server.problem !== undefined && (
        <Typography variant="bodySmall" color="error" sx={{ pl: 4 }}>
          {t('workspace.mcp.proposalProblem', 'Cannot be added: {{problem}}', { problem: server.problem })}
        </Typography>
      )}
    </Box>
  );
}

function ProposalCard({ proposal, busy, onAnswer }: { proposal: McpProposal; busy: boolean; onAnswer: (accept: string[]) => void }): React.JSX.Element {
  const [picked, setPicked] = useState<string[]>([]);
  const toggle = (name: string): void => setPicked((current) => (current.includes(name) ? current.filter((x) => x !== name) : [...current, name]));
  return (
    <Alert severity="info" data-testid="mcp-proposal" icon={false}>
      <Stack spacing={1}>
        <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
          {t('workspace.mcp.proposalTitle', 'This repository proposes MCP servers ({{file}})', { file: proposal.file })}
        </Typography>
        <Typography variant="bodySmall">
          {t('workspace.mcp.proposalHelp', 'Nothing runs from the file. The servers you add are copied into this workspace’s settings and start with the next turn, under the sandbox and without network.')}
        </Typography>
        <Stack component="ul" spacing={0.5} sx={{ listStyle: 'none', p: 0, m: 0 }}>
          {proposal.servers.map((server) => (
            <ProposedServer key={server.name} server={server} checked={picked.includes(server.name)} onToggle={() => toggle(server.name)} />
          ))}
        </Stack>
        <Stack direction="row" spacing={1}>
          <Button size="small" variant="contained" disabled={busy || picked.length === 0} onClick={() => onAnswer(picked)}>
            {t('workspace.mcp.proposalAdd', 'Add the selected servers')}
          </Button>
          <Button size="small" disabled={busy} onClick={() => onAnswer([])}>
            {t('workspace.mcp.proposalDecline', 'Not now')}
          </Button>
        </Stack>
      </Stack>
    </Alert>
  );
}

export interface McpProposalsProps {
  workspaceId: string;
  /** Tests pass a fake; the app uses the host's. */
  ipc?: McpIpc;
}

export function McpProposals({ workspaceId, ipc: given }: McpProposalsProps): React.JSX.Element | null {
  const ipc = useMemo(() => given ?? tauriMcpIpc(), [given]);
  const queryClient = useQueryClient();
  const key = mcpListKey(workspaceId);
  const list = useQuery({ queryKey: key, queryFn: () => ipc?.list(workspaceId) ?? Promise.reject(new Error('no host')), enabled: ipc !== undefined });
  const [failure, setFailure] = useState<string | null>(null);
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let live = true;
    let unlisten: () => void = () => undefined;
    void ipc
      .onProposal((event) => {
        if (event.workspace_id === workspaceId) void queryClient.invalidateQueries({ queryKey: mcpListKey(workspaceId) });
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
