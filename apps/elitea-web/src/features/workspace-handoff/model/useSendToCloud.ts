/**
 * The send dialog's state: the preview (re-read when a file is left on the
 * laptop, or the folder's settings change), the files left, the folder
 * settings the dialog changes in place ("Include anyway", "Remove include",
 * the env checklist), the confirmation the included files' findings need,
 * and the send itself with the host's progress (`handoff://event`) and its
 * cancel. A failed send keeps its rejection: findings, the caps breakdown and
 * quota figures are shown from it.
 *
 * The host's preview already honours `leave_on_laptop` (a file left behind
 * and its findings are gone from it); the only findings merged in here are a
 * `secret_found` rejection's, de-duplicated against the preview's.
 */
import { useEffect, useMemo, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import type { FolderHandoffSettings, HandoffEvent, HandoffIpc, HandoffPreview, HandoffSent, SecretFinding } from '@/shared/desktop/handoffIpc';
import { HandoffIpcError } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { findingsOf } from './describeHandoffError';

export interface SendToCloud {
  preview: HandoffPreview | undefined;
  previewError: unknown;
  loading: boolean;
  /** Secret findings that block the send. */
  findings: SecretFinding[];
  leave: readonly string[];
  leaveOnLaptop: (path: string) => void;
  /** Persist an exact path into the folder's includes, then preview again. */
  include: (path: string) => void;
  removeInclude: (path: string) => void;
  /** Select or unselect an environment variable (by name) in the folder's settings. */
  toggleEnv: (name: string) => void;
  /** A settings change is being saved. */
  saving: boolean;
  settingsError: unknown;
  /** The person confirmed the include warnings of the preview shown now (a new preview asks again). */
  includesConfirmed: boolean;
  confirmIncludes: (confirmed: boolean) => void;
  progress: HandoffEvent | null;
  sending: boolean;
  sendError: unknown;
  send: () => void;
  cancelling: boolean;
  cancel: () => void;
}

const previewKey = (workspaceId: string) => ['workspace', 'handoff', 'preview', workspaceId] as const;

/** The settings a change persists, read fresh from the host first (another window may have changed them). */
async function updateSettings(ipc: HandoffIpc, workspaceId: string, change: (settings: FolderHandoffSettings) => FolderHandoffSettings): Promise<void> {
  const current = await ipc.settingsGet(workspaceId);
  await ipc.settingsSet(workspaceId, change(current.settings));
}

const without = (list: readonly string[], value: string): string[] => list.filter((entry) => entry !== value);

export function useSendToCloud(ipc: HandoffIpc, workspaceId: string, conversationUuid: string, onSent: (sent: HandoffSent) => void): SendToCloud {
  const queryClient = useQueryClient();
  const [leave, setLeave] = useState<readonly string[]>([]);
  const [progress, setProgress] = useState<HandoffEvent | null>(null);
  // The preview (by its update time) the person confirmed the include warnings for.
  const [confirmedAt, setConfirmedAt] = useState<number | null>(null);

  const query = useQuery({
    queryKey: [...previewKey(workspaceId), conversationUuid, [...leave].sort()],
    queryFn: () => ipc.preview(workspaceId, conversationUuid, leave),
    retry: false,
    // A preview is a snapshot of the folder at one moment: never reuse an old one.
    gcTime: 0,
    staleTime: 0,
    refetchOnWindowFocus: false,
    placeholderData: (previous) => previous,
  });

  useEffect(() => {
    let unlisten: (() => void) | undefined;
    let disposed = false;
    void ipc
      .onEvent((event) => {
        if (event.conversation_uuid.toLowerCase() === conversationUuid.toLowerCase()) setProgress(event);
      })
      .then((off) => {
        if (disposed) off();
        else unlisten = off;
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [ipc, conversationUuid]);

  const includesConfirmed = confirmedAt !== null && confirmedAt === query.dataUpdatedAt && (query.data?.include_findings.length ?? 0) > 0;

  const mutation = useMutation({
    mutationFn: () => {
      setProgress(null);
      return ipc.send(workspaceId, conversationUuid, leave, includesConfirmed ? { confirmIncludes: true } : undefined);
    },
    onSuccess: onSent,
  });

  const settings = useMutation({
    mutationFn: (change: (current: FolderHandoffSettings) => FolderHandoffSettings) => updateSettings(ipc, workspaceId, change),
    onMutate: () => mutation.reset(),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: previewKey(workspaceId) }),
  });

  const cancel = useMutation({ mutationFn: () => ipc.cancel(conversationUuid) });

  const findings = useMemo(() => {
    const found = [...(query.data?.findings ?? []), ...findingsOf(mutation.error)];
    const seen = new Set<string>();
    return found.filter((finding) => {
      const key = `${finding.path}:${String(finding.line)}:${finding.rule}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }, [query.data, mutation.error]);

  return {
    preview: query.data,
    previewError: query.error,
    loading: query.isPending || query.isFetching,
    findings,
    leave,
    leaveOnLaptop: (path) => {
      mutation.reset();
      setLeave((current) => (current.includes(path) ? current : [...current, path]));
    },
    include: (path) => settings.mutate((current) => (current.include.includes(path) ? current : { ...current, include: [...current.include, path] })),
    removeInclude: (path) =>
      settings.mutate((current) => {
        const next = without(without(current.include, path), `/${path}`);
        if (next.length === current.include.length) {
          throw new HandoffIpcError('included_by_pattern', t('workspace.handoff.includedByPattern', 'This file is included by a pattern. Change the pattern in Handoff settings.'));
        }
        return { ...current, include: next };
      }),
    toggleEnv: (name) =>
      settings.mutate((current) => ({ ...current, env_names: current.env_names.includes(name) ? without(current.env_names, name) : [...current.env_names, name] })),
    saving: settings.isPending,
    settingsError: settings.error,
    includesConfirmed,
    confirmIncludes: (confirmed) => setConfirmedAt(confirmed ? query.dataUpdatedAt : null),
    progress,
    sending: mutation.isPending,
    sendError: mutation.error,
    send: () => {
      cancel.reset();
      mutation.mutate();
    },
    cancelling: cancel.isPending,
    cancel: () => cancel.mutate(),
  };
}
