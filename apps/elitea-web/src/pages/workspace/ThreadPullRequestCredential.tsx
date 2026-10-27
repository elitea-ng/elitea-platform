/**
 * The desktop's cloud-thread banner: the push and pull-request credential
 * picker (ADR-0034 decision 10, amended 2026-10-26), reading the thread's
 * current choice itself (the desktop has no chat-page handoff read). A
 * failure is said inline: the banner has no toast.
 */
import { useState } from 'react';

import { useQuery, useQueryClient } from '@tanstack/react-query';

import Typography from '@mui/material/Typography';

import { CloudPullRequestCredential } from '@/features/forge-credential';
import { handoffInfoKey } from '@/features/workspace-handoff';
import { getWorkspaceHandoff } from '@/shared/api/workspaceHandoffApi';

export function ThreadPullRequestCredential({ projectId, conversationUuid }: { readonly projectId: string; readonly conversationUuid: string }): React.JSX.Element | null {
  const queryClient = useQueryClient();
  const [message, setMessage] = useState<string | null>(null);
  const info = useQuery({
    queryKey: handoffInfoKey(projectId, conversationUuid),
    queryFn: () => getWorkspaceHandoff(projectId, conversationUuid),
    retry: false,
  });
  if (info.data === undefined) return null;
  return (
    <>
      <CloudPullRequestCredential
        projectId={projectId}
        conversationUuid={conversationUuid}
        current={info.data.pull_request_credential}
        onSaved={() => {
          setMessage(null);
          void queryClient.invalidateQueries({ queryKey: handoffInfoKey(projectId, conversationUuid) });
        }}
        onError={setMessage}
      />
      {message !== null && (
        <Typography variant="bodySmall" role="alert" data-testid="cloud-pr-credential-error">
          {message}
        </Typography>
      )}
    </>
  );
}
