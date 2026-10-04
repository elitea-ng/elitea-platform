import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { PipelineInboundTrigger } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import {
  AGENT_WEBHOOK_MODES,
  agentWebhookModeOf,
  useAgentWebhookTrigger,
  type AgentWebhookMode,
  type AgentWebhookTrigger,
} from '../lib/useAgentWebhookTrigger';

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.5rem' };
const valueSx: SxProps<Theme> = {
  wordBreak: 'break-all',
  padding: '0.5rem',
  borderRadius: (theme: Theme) => theme.vars.shape.radiusMd,
  backgroundColor: 'background.default',
  color: 'text.secondary',
};

const MODE_OPTIONS = AGENT_WEBHOOK_MODES.map(mode => ({ label: mode.label, value: mode.value }));

/** What a sender of each type must be configured with. The strings are the pipeline dialog's, by key. */
function modeHint(mode: AgentWebhookMode): string {
  switch (mode) {
    case 'github':
      return t(
        'pipelines.pipelineWebhookModal.modeGithubHint',
        'GitHub signs the request body with the secret and sends the digest in {{header}}. Configure that secret in the repository’s webhook settings; no Authorization header is sent or accepted.',
        { header: 'X-Hub-Signature-256' },
      );
    case 'gitlab':
      return t(
        'pipelines.pipelineWebhookModal.modeGitlabHint',
        'GitLab sends the secret token verbatim in {{header}}. Paste the secret into the webhook’s Secret token field in GitLab.',
        { header: 'X-Gitlab-Token' },
      );
    case 'gitlab_signing':
      return t(
        'pipelines.pipelineWebhookModal.modeGitlabSigningHint',
        'GitLab signs webhook-id, webhook-timestamp and the request body with the signing token and sends it in webhook-signature. Deliveries older than five minutes are refused.',
      );
    case 'custom':
      return t(
        'pipelines.pipelineWebhookModal.modeCustomHint',
        'The sender presents the secret itself, as an Authorization: Bearer header, as X-Elitea-Trigger-Token, or as a token query parameter.',
      );
  }
}

interface TriggerState {
  readonly configured: boolean;
  readonly revoked: boolean;
  readonly live: boolean;
  readonly disabled: boolean;
  readonly modeChanged: boolean;
}

function triggerState(settings: AgentWebhookTrigger, isReadOnly: boolean, mode: AgentWebhookMode): TriggerState {
  const configured = settings.trigger?.configured === true;
  const revoked = configured && typeof settings.trigger?.revoked_at === 'string';
  return {
    configured,
    revoked,
    live: configured && !revoked,
    disabled: settings.isBusy || isReadOnly,
    modeChanged: configured && !revoked && mode !== agentWebhookModeOf(settings.trigger),
  };
}

function fullUrl(trigger: PipelineInboundTrigger | undefined): string {
  return trigger?.url === undefined ? '' : new URL(trigger.url, window.location.origin).toString();
}

interface TriggerActionsProps {
  readonly settings: AgentWebhookTrigger;
  readonly state: TriggerState;
  readonly mode: AgentWebhookMode;
}

function TriggerActions({ settings, state, mode }: TriggerActionsProps): ReactNode {
  const { secret } = settings;
  return (
    <Box sx={actionsSx}>
      <Button variant="contained" size="small" disabled={state.disabled} onClick={() => void settings.rotate(mode)} data-testid="agent-trigger-rotate">
        {state.live
          ? t('pages.pipelines.editPipeline.triggers.trigger.rotate', 'Rotate')
          : t('pages.pipelines.editPipeline.triggers.trigger.create', 'Create trigger URL')}
      </Button>
      {state.live && secret === undefined && (
        <Button variant="outlined" size="small" disabled={state.disabled} onClick={() => void settings.reveal()} data-testid="agent-trigger-reveal">
          {t('pipelines.pipelineWebhookModal.reveal', 'Reveal secret')}
        </Button>
      )}
      {secret !== undefined && (
        <Button variant="outlined" size="small" onClick={() => void handleCopy(secret)} data-testid="agent-trigger-copy">
          {t('pages.pipelines.editPipeline.triggers.trigger.copy', 'Copy')}
        </Button>
      )}
      {secret !== undefined && (
        <Button variant="text" size="small" onClick={settings.hideSecret} data-testid="agent-trigger-hide">
          {t('pages.pipelines.editPipeline.triggers.trigger.hide', 'Hide')}
        </Button>
      )}
      {state.live && (
        <Button variant="outlined" color="error" size="small" disabled={state.disabled} onClick={() => void settings.revoke()} data-testid="agent-trigger-revoke">
          {t('pages.pipelines.editPipeline.triggers.trigger.revoke', 'Revoke')}
        </Button>
      )}
    </Box>
  );
}

function TriggerBody({ settings, isReadOnly }: { readonly settings: AgentWebhookTrigger; readonly isReadOnly: boolean }): ReactNode {
  const storedMode = agentWebhookModeOf(settings.trigger);
  const [mode, setMode] = useState<AgentWebhookMode>(storedMode);
  // A rotate answers with the stored mode; the selection follows it.
  useEffect(() => setMode(storedMode), [storedMode]);
  const state = triggerState(settings, isReadOnly, mode);
  const lastUsed = settings.trigger?.last_used_at;

  return (
    <Box sx={sectionSx} data-testid="agent-trigger-card">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.agents.editApplication.triggers.description',
          'An external system starts this agent by calling this URL with its secret. The body’s `input`, or else the payload, is the message.',
        )}
      </Typography>
      {settings.error !== undefined && (
        <Typography variant="bodySmall" color="error" data-testid="agent-triggers-error">
          {t('pages.pipelines.editPipeline.triggers.error', 'That change could not be saved. Try again.')}
        </Typography>
      )}
      <SingleSelect
        id="agent-trigger-mode"
        label={t('pipelines.pipelineWebhookModal.modeLabel', 'Webhook type')}
        value={mode}
        options={MODE_OPTIONS}
        onChange={value => setMode(AGENT_WEBHOOK_MODES.find(option => option.value === value)?.value ?? 'custom')}
        disabled={state.disabled}
      />
      <Typography variant="bodySmall" color="text.secondary" data-testid="agent-trigger-mode-hint">
        {modeHint(mode)}
      </Typography>
      {state.modeChanged && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-trigger-mode-pending">
          {t(
            'pipelines.pipelineWebhookModal.modePending',
            'Applying this type rotates the credential: the current secret stops working and the sender has to be given the new one.',
          )}
        </Typography>
      )}
      {!state.configured && (
        <Typography variant="bodyMedium" data-testid="agent-trigger-absent">
          {t('pages.agents.editApplication.triggers.absent', 'This agent has no trigger URL.')}
        </Typography>
      )}
      {state.configured && (
        <Typography variant="bodySmall" sx={valueSx} data-testid="agent-trigger-url">
          {fullUrl(settings.trigger)}
        </Typography>
      )}
      {settings.secret !== undefined && (
        <Typography variant="bodySmall" sx={valueSx} data-testid="agent-trigger-secret">
          {settings.secret}
        </Typography>
      )}
      {state.revoked && (
        <Typography variant="bodySmall" color="error" data-testid="agent-trigger-revoked">
          {t('pages.pipelines.editPipeline.triggers.trigger.revoked', 'This trigger is revoked and refuses every call.')}
        </Typography>
      )}
      {state.live && typeof lastUsed === 'string' && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-trigger-last-used">
          {t('pages.pipelines.editPipeline.triggers.trigger.lastUsed', 'Last called: {{when}}', { when: new Date(lastUsed).toLocaleString() })}
        </Typography>
      )}
      <TriggerActions settings={settings} state={state} mode={mode} />
    </Box>
  );
}

export interface EditApplicationTriggersPanelProps {
  readonly projectId: string | undefined;
  readonly versionId: number | undefined;
  readonly isReadOnly: boolean;
}

/**
 * "Triggers" — the agent's inbound webhook (legacy issue 6656), the agent
 * twin of the pipeline editor's "Triggers & schedules" section.
 *
 * An agent version has the trigger only. The schedule stays a pipeline
 * facility: an agent answers a message, and a cron has none to give it.
 *
 * The credential is shown only after a create, a rotate or an explicit
 * reveal, and is never part of the read that populates this section. The
 * reveal carries the write permission on the server, so a view-only member
 * cannot copy a working credential. A change of type rotates the credential,
 * because the server writes the type on the rotate route and nowhere else.
 *
 * The section renders nothing until the version is resolved: a "create"
 * button for a version id the page does not have could not work.
 */
export function EditApplicationTriggersPanel({ projectId, versionId, isReadOnly }: EditApplicationTriggersPanelProps): ReactNode {
  const settings = useAgentWebhookTrigger(projectId, versionId);
  const items = useMemo(
    () => [
      {
        title: t('pages.agents.editApplication.triggers.title', 'Triggers'),
        content: <TriggerBody settings={settings} isReadOnly={isReadOnly} />,
      },
    ],
    [settings, isReadOnly],
  );
  if (projectId === undefined || versionId === undefined) return null;
  return <BasicAccordion items={items} data-testid="edit-application-triggers-panel" />;
}
