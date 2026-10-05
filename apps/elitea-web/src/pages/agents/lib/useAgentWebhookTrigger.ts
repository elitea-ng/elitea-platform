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
 *  - the credential is component state, never query cache, and it belongs to
 *    ONE version: it is tagged with the project and version it was minted or
 *    revealed for, and is not shown once the editor moves to another version.
 *    The panel stays mounted across a version switch, so untagged state showed
 *    version A's secret beside version B's URL;
 *  - a rotate in the stored mode sends NO body, so the server keeps the stored
 *    mode, header, event list and variable opt-in. A body is sent only to
 *    create a trigger or to change its mode.
 *
 * It replaces the agent's old "Webhook secret" model setting. That field
 * wrote `llm_settings.webhook_secret`, which no server code read, so a value
 * typed into it authenticated nothing.
 */

/**
 * The sender types, by what the backend stores. `custom_hmac` is a trigger
 * minted through the API with `auth_mode: hmac_sha256` and a header of its
 * own. This section cannot create one (it has no header field), but it must
 * not misread one as GitHub: a GitHub rotate would rewrite its header to
 * `X-Hub-Signature-256`, and the sender's next deliveries would be refused.
 */
export type AgentWebhookMode = 'custom' | 'github' | 'gitlab' | 'gitlab_signing' | 'custom_hmac';

const GITHUB_SIGNATURE_HEADER = 'x-hub-signature-256';

export const AGENT_WEBHOOK_MODES: readonly { readonly value: AgentWebhookMode; readonly label: string }[] = [
  { value: 'custom', label: 'Custom (bearer secret)' },
  { value: 'github', label: 'GitHub (signed payload)' },
  { value: 'gitlab', label: 'GitLab (secret token)' },
  { value: 'gitlab_signing', label: 'GitLab (signing token)' },
  { value: 'custom_hmac', label: 'Custom (signed payload)' },
];

/** The modes a person may select: `custom_hmac` only when the trigger already is one. */
export function selectableAgentWebhookModes(stored: AgentWebhookMode): typeof AGENT_WEBHOOK_MODES {
  return AGENT_WEBHOOK_MODES.filter(mode => mode.value !== 'custom_hmac' || stored === 'custom_hmac');
}

/**
 * The create/rotate body for a mode. The backend writes the mode on that route
 * and nowhere else. `custom_hmac` has no body of its own: it is only ever
 * rotated in place, with no body, so the server keeps its header.
 */
export function agentWebhookModeRequest(mode: AgentWebhookMode): PipelineInboundTriggerModeRequest | undefined {
  if (mode === 'custom_hmac') return undefined;
  if (mode === 'gitlab_signing') return { type: 'gitlab', auth_mode: 'standard_webhooks_hmac' };
  return { type: mode };
}

/** The stored row as a mode. A row with no mode is Custom. */
export function agentWebhookModeOf(trigger: PipelineInboundTrigger | undefined): AgentWebhookMode {
  if (trigger?.auth_mode === 'hmac_sha256') {
    const header = (trigger.signature_header ?? '').toLowerCase();
    return trigger.provider === 'github' || header === GITHUB_SIGNATURE_HEADER ? 'github' : 'custom_hmac';
  }
  if (trigger?.auth_mode === 'standard_webhooks_hmac') return 'gitlab_signing';
  return trigger?.provider === 'gitlab' ? 'gitlab' : 'custom';
}

/**
 * The body a rotate sends: none when the trigger is live in the selected mode
 * (the server then keeps every stored setting), and the mode's body otherwise.
 */
export function agentWebhookRotateRequest(
  trigger: PipelineInboundTrigger | undefined,
  mode: AgentWebhookMode,
): PipelineInboundTriggerModeRequest | undefined {
  if (trigger?.configured === true && agentWebhookModeOf(trigger) === mode) return undefined;
  return agentWebhookModeRequest(mode);
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

/** A value that belongs to one project and version. */
interface Scoped {
  readonly scope: string;
  readonly value: string | undefined;
}

export function useAgentWebhookTrigger(projectIdText: string | undefined, versionId: number | undefined): AgentWebhookTrigger {
  const projectId = numericProjectId(projectIdText) ?? 0;
  const version = versionId ?? 0;
  const enabled = projectId > 0 && version > 0;
  const scope = `${projectId}:${version}`;
  const queryClient = useQueryClient();
  // Tagged with the version they were produced for. A value from another
  // version is never returned, so a version switch hides it at once, and an
  // answer that arrives after the switch is never shown at all.
  const [secret, setSecret] = useState<Scoped | undefined>(undefined);
  const [error, setError] = useState<Scoped | undefined>(undefined);
  const [isBusy, setIsBusy] = useState(false);

  const query = useGetPipelineInboundTrigger(projectId, version, { query: { enabled, retry: false } });
  const envelope = query.data;
  const trigger = envelope?.status === 200 ? envelope.data : undefined;

  const run = useCallback(
    async (action: () => Promise<{ readonly status: number; readonly data: unknown } | undefined>): Promise<void> => {
      if (!enabled) return;
      setIsBusy(true);
      setError(undefined);
      try {
        const answer = await action();
        const body = answer?.status === 200 ? (answer.data as PipelineInboundTrigger) : undefined;
        setSecret({ scope, value: body?.secret });
        await queryClient.invalidateQueries({ queryKey: getGetPipelineInboundTriggerQueryKey(projectId, version) });
      } catch (cause) {
        setError({ scope, value: cause instanceof Error ? cause.message : String(cause) });
      } finally {
        setIsBusy(false);
      }
    },
    [enabled, queryClient, projectId, version, scope],
  );

  const rotate = useCallback(
    (mode: AgentWebhookMode) => run(() => rotatePipelineInboundTrigger(projectId, version, agentWebhookRotateRequest(trigger, mode))),
    [run, projectId, version, trigger],
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

  return {
    trigger,
    secret: secret?.scope === scope ? secret.value : undefined,
    error: error?.scope === scope ? error.value : undefined,
    isBusy,
    rotate,
    reveal,
    revoke,
    hideSecret,
  };
}
