import { useQuery, useQueryClient } from '@tanstack/react-query';
import { router, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import { isApiError } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';
import { type ForgeCredential, type HandoffInfo, setPullRequestCredential } from '@/api/queries/workspaceHandoff';
import { ForgeCredentialSheet } from '@/features/localWork/ForgeCredentialSheet';
import { credentialFailure, loadCredentialChoices } from '@/features/localWork/forgeCredential';
import { usePersonalProjectId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';

/**
 * A cloud thread's push and pull-request credential (client contract 1.29), a native sheet over
 * the thread: the owner chooses a GitHub or GitLab configuration (the project's, a shared one,
 * or their own). Only the reference is sent; the handoff state is read again after a save.
 */
export default function PullRequestCredentialScreen() {
  const params = useLocalSearchParams<{ uuid?: string }>();
  const uuid = typeof params.uuid === 'string' && params.uuid !== '' ? params.uuid : null;
  const projectId = useSession((s) => s.projectId);
  const personalId = usePersonalProjectId();
  const queryClient = useQueryClient();
  const enabled = projectId !== null && uuid !== null;
  const choices = useQuery({
    queryKey: queryKeys.forgeCredentials(projectId ?? 0, personalId),
    enabled,
    queryFn: ({ signal }) => loadCredentialChoices(projectId as number, personalId, signal),
  });
  const current = enabled ? (queryClient.getQueryData<HandoffInfo>(queryKeys.handoff(projectId, uuid))?.pull_request_credential ?? null) : null;
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(enabled ? null : 'This thread cannot choose a credential here.');

  const choose = (credential: ForgeCredential | null) => {
    if (!enabled || saving) return;
    setSaving(true);
    setError(null);
    setPullRequestCredential(projectId, uuid, credential)
      .then(() => {
        void queryClient.invalidateQueries({ queryKey: queryKeys.handoff(projectId, uuid) });
        if (router.canGoBack()) router.back();
      })
      .catch((failure: unknown) => setError(credentialFailure(isApiError(failure) && failure.kind === 'http' ? failure.status : undefined)))
      .finally(() => setSaving(false));
  };

  return (
    <ForgeCredentialSheet
      loading={enabled && choices.isPending}
      failed={choices.error != null}
      onRetry={() => void choices.refetch()}
      choices={choices.data ?? []}
      current={current}
      saving={saving}
      error={error}
      onChoose={choose}
    />
  );
}
