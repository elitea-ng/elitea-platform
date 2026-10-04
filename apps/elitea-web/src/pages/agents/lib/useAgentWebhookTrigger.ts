import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import {
  getGetPipelineInboundTriggerQueryKey,
  revealPipelineInboundTrigger,
  revokePipelineInboundTrigger,
  rotatePipelineInboundTrigger,
  useGetPipelineInboundTrigger,
} from '@/shared/api/generated/applications/applications';
import type { PipelineInboundTrigger, PipelineInboundTriggerModeRequest } from '@/shared/api/generated/model';

/**
 * The data half of the agent editor's Triggers section (legacy issue 6656).
 *
 * An AGENT version uses the same inbound trigger as a pipeline: the same
 * `/pipeline_triggers/…` settings routes, the same minted credential, the same
 * GitHub and GitLab modes. Only the version id differs. So this hook calls the
 * generated client the pipeline editor calls, and has the same three rules
 * (see `pages/pipelines/lib/usePipelineTriggerSettings.ts`):
 *
 *  - the read is unwrapped ONE `.data` down, because `eliteaFetch` resolves
 *    the response envelope (issue 132);
 *  - the writes are plain calls plus an invalidation, never query hooks;
 *  - the credential is component state, never query cache.
 *
 * It replaces the agent's old "Webhook secret" model setting. That field
 * wrote `llm_settings.webhook_secret`, which no server code read, so a value
 * typed into it authenticated nothing.
 */

/** The four sender types, by what the backend stores. */
export type AgentWebhookMode = 'custom' | 'github' | 'gitlab' | 'gitlab_signing';

export const AGENT_WEBHOOK_MODES: readonly { readonly value: AgentWebhookMode; readonly label: string }[] = [
  { value: 'custom', label: 'Custom (bearer secret)' },
  { value: 'github', label: 'GitHub (signed payload)' },
  { value: 'gitlab', label: 'GitLab (secret token)' },
  { value: 'gitlab_signing', label: 'GitLab (signing token)' },
];

/** The create/rotate body for a mode. The backend writes the mode on that route and nowhere else. */
export function agentWebhookModeRequest(mode: AgentWebhookMode): PipelineInboundTriggerModeRequest {
  if (mode === 'gitlab_signing') return { type: 'gitlab', auth_mode: 'standard_webhooks_hmac' };
  return { type: mode };
}

/** The stored row as a mode. A row with no mode is Custom. */
export function agentWebhookModeOf(trigger: PipelineInboundTrigger | undefined): AgentWebhookMode {
  if (trigger?.auth_mode === 'hmac_sha256') return 'github';
  if (trigger?.auth_mode === 'standard_webhooks_hmac') return 'gitlab_signing';
  return trigger?.provider === 'gitlab' ? 'gitlab' : 'custom';
}

export interface AgentWebhookTrigger {
  readonly trigger: PipelineInboundTrigger | undefined;
  /** The credential, present only after a create/rotate or an explicit reveal. */
  readonly secret: string | undefined;
  readonly error: string | undefined;
  readonly isBusy: boolean;
  readonly rotate: (mode: AgentWebhookMode) => Promise<void>;
  readonly reveal: () => Promise<void>;
  readonly revoke: () => Promise<void>;
  readonly hideSecret: () => void;
}

function numericProjectId(projectId: string | undefined): number | undefined {
  if (projectId === undefined || projectId === '') return undefined;
  const parsed = Number(projectId);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : undefined;
}

export function useAgentWebhookTrigger(projectIdText: string | undefined, versionId: number | undefined): AgentWebhookTrigger {
  const projectId = numericProjectId(projectIdText) ?? 0;
  const version = versionId ?? 0;
  const enabled = projectId > 0 && version > 0;
  const queryClient = useQueryClient();
  const [secret, setSecret] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const [isBusy, setIsBusy] = useState(false);

  const query = useGetPipelineInboundTrigger(projectId, version, { query: { enabled, retry: false } });

  const run = useCallback(
    async (action: () => Promise<{ readonly status: number; readonly data: unknown } | undefined>): Promise<void> => {
      if (!enabled) return;
      setIsBusy(true);
      setError(undefined);
      try {
        const answer = await action();
        const body = answer?.status === 200 ? (answer.data as PipelineInboundTrigger) : undefined;
        setSecret(body?.secret);
        await queryClient.invalidateQueries({ queryKey: getGetPipelineInboundTriggerQueryKey(projectId, version) });
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setIsBusy(false);
      }
    },
    [enabled, queryClient, projectId, version],
  );

  const rotate = useCallback(
    (mode: AgentWebhookMode) => run(() => rotatePipelineInboundTrigger(projectId, version, agentWebhookModeRequest(mode))),
    [run, projectId, version],
  );
  const reveal = useCallback(() => run(() => revealPipelineInboundTrigger(projectId, version)), [run, projectId, version]);
  const revoke = useCallback(
    () =>
      run(async () => {
        await revokePipelineInboundTrigger(projectId, version);
        return undefined;
      }),
    [run, projectId, version],
  );
  const hideSecret = useCallback(() => setSecret(undefined), []);

  const envelope = query.data;
  return {
    trigger: envelope?.status === 200 ? envelope.data : undefined,
    secret,
    error,
    isBusy,
    rotate,
    reveal,
    revoke,
    hideSecret,
  };
}
