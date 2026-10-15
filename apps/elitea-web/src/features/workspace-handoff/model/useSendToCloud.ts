/**
 * The send dialog's state: the preview (re-read when a file is left on the
 * laptop), the files left, and the send itself with the host's progress
 * (`handoff://event`). A failed send keeps its rejection: findings, the caps
 * breakdown and quota figures are shown from it.
 */
import { useEffect, useMemo, useState } from 'react';

import { useMutation, useQuery } from '@tanstack/react-query';

import type { HandoffEvent, HandoffIpc, HandoffPreview, HandoffSent, SecretFinding } from '@/shared/desktop/handoffIpc';

import { findingsOf } from './describeHandoffError';

export interface SendToCloud {
  preview: HandoffPreview | undefined;
  previewError: unknown;
  loading: boolean;
  /** Secret findings that still block the send (a file left on the laptop no longer does). */
  findings: SecretFinding[];
  leave: readonly string[];
  leaveOnLaptop: (path: string) => void;
  progress: HandoffEvent | null;
  sending: boolean;
  sendError: unknown;
  send: () => void;
}

export function useSendToCloud(ipc: HandoffIpc, workspaceId: string, conversationUuid: string, onSent: (sent: HandoffSent) => void): SendToCloud {
  const [leave, setLeave] = useState<readonly string[]>([]);
  const [progress, setProgress] = useState<HandoffEvent | null>(null);

  const query = useQuery({
    queryKey: ['workspace', 'handoff', 'preview', workspaceId, conversationUuid, [...leave].sort()],
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

  const mutation = useMutation({
    mutationFn: () => {
      setProgress(null);
      return ipc.send(workspaceId, conversationUuid, leave);
    },
    onSuccess: onSent,
  });

  const findings = useMemo(() => {
    const left = new Set(leave);
    const found = [...(query.data?.findings ?? []), ...findingsOf(mutation.error)];
    const seen = new Set<string>();
    return found.filter((finding) => {
      const key = `${finding.path}:${String(finding.line)}:${finding.rule}`;
      if (left.has(finding.path) || seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }, [query.data, mutation.error, leave]);

  return {
    preview: query.data,
    previewError: query.error,
    loading: query.isPending || (query.isPlaceholderData && query.isFetching),
    findings,
    leave,
    leaveOnLaptop: (path) => {
      mutation.reset();
      setLeave((current) => (current.includes(path) ? current : [...current, path]));
    },
    progress,
    sending: mutation.isPending,
    sendError: mutation.error,
    send: mutation.mutate,
  };
}
