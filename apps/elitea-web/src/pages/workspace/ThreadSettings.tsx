/**
 * The thread's settings in its Participants tab (#3145), each with its value, instead of chips in the composer footer
 * and the header: the composer keeps attach, the mode, "To:", "To completion" and Send.
 *
 * - Workspace: where a new thread works ("Work in": this folder or a new task worktree, `WorkInChip`), and where the
 *   thread runs, with the actions that move it ("Send to cloud", `RunLocationControl`).
 * - Sandbox: the folder's sandbox state ("No sandbox" / "Trust paused", `TrustBadge`, with Revoke), else "Sandboxed".
 * - Sub-agents: the read-only sub-agents the agents may start (always offered here; nothing to switch).
 * - Step limit: the model steps each run here may take (`StepLimitMenu`; `/steps` sets the same).
 * - Models: each agent's model in this thread (`AgentModelRow`), its own unless chosen here (`agentModels`).
 * - Context and index: the language servers, the folder's index and the context ring.
 *
 * "Respect project instructions" follows in the tab (`ProjectInstructionsSection`), under the same heading. The composer renders
 * this into the tab (a portal), so its controls stay in the composer's state.
 */
import { useEffect, useRef, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { TrustBadge, useFolderTrustBadged } from '@/features/folder-trust';
import { composer } from '@/features/workspace';
import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';

import type { AgentModels } from './agentModels';
import StepLimitMenu, { StepLimitValue } from './StepLimitMenu';
import { useAgentModelOptions } from './useTurnModel';

/** One agent whose model the thread may choose: joined, or joining with the next message. */
export interface ModelAgent {
  readonly applicationId: number;
  /** The version it runs; 0: its default one (its own model is then not shown). */
  readonly versionId: number;
  readonly name: string;
  readonly cloud: boolean;
}

function SettingRow({ label, testId, children }: { readonly label: string; readonly testId: string; readonly children: ReactNode }): React.JSX.Element {
  return (
    <Box data-testid={testId} sx={{ display: 'flex', alignItems: 'center', gap: 1, minHeight: '1.75rem', minWidth: 0 }}>
      <Typography variant="labelSmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, width: '6.5rem', flexShrink: 0 })}>
        {label}
      </Typography>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap', minWidth: 0, flex: 1 }}>{children}</Box>
    </Box>
  );
}

function Value({ children, testId }: { readonly children: ReactNode; readonly testId?: string }): React.JSX.Element {
  return (
    <Typography variant="labelSmall" data-testid={testId} sx={{ paddingX: 0.75, minWidth: 0 }} noWrap>
      {children}
    </Typography>
  );
}

function SandboxValue({ workspaceId }: { readonly workspaceId: string }): React.JSX.Element {
  // A trusted folder (or one whose trust is paused) carries its badge, with Revoke; an untrusted one runs its commands
  // in the sandbox. A worktree's state is its parent's (`inherited_from`).
  return useFolderTrustBadged(workspaceId) ? <TrustBadge workspaceId={workspaceId} /> : <Value testId="thread-settings-sandboxed">{t('workspace.settings.sandboxed', 'Sandboxed')}</Value>;
}

interface AgentModelRowProps {
  readonly agent: ModelAgent;
  readonly models: AgentModels;
  readonly workspaceId: string;
  readonly projectId: number;
}

/**
 * One agent's model in this thread: its own model, or the one chosen here (`ModelChip`, with "the agent's own model"
 * to go back). A cloud agent, an agent that pins its model and a folder whose settings keep the models show the value
 * only. Its row's "Model in this thread…" in the Participants list (and `/model`) opens its menu here.
 */
function AgentModelRow({ agent, models, workspaceId, projectId }: AgentModelRowProps): React.JSX.Element {
  const options = useAgentModelOptions(workspaceId, projectId, agent.cloud ? 0 : agent.applicationId, agent.versionId);
  const box = useRef<HTMLDivElement>(null);
  const editing = models.editing === agent.applicationId;
  const { edit } = models;
  useEffect(() => {
    if (!editing) return;
    edit(null);
    box.current?.scrollIntoView?.({ block: 'nearest' });
    box.current?.querySelector<HTMLElement>('button')?.click();
  }, [editing, edit]);
  const chosen = models.overrideOf(agent.applicationId);
  const current = chosen?.name ?? options.own;
  const effort = chosen === undefined ? options.ownEffort : chosen.reasoning_effort;
  return (
    <Box ref={box} data-testid="thread-settings-model" data-agent-id={agent.applicationId} data-overridden={chosen === undefined ? 'false' : 'true'} sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }}>
      <Typography variant="labelSmall" noWrap sx={{ minWidth: 0, maxWidth: '7rem' }} title={agent.name}>
        {agent.name}
      </Typography>
      {agent.cloud ? (
        <Value>{t('workspace.settings.modelCloud', 'Its own model (cloud)')}</Value>
      ) : options.offered ? (
        <composer.ModelChip
          current={current}
          effort={effort}
          models={options.models}
          own={options.own}
          onChoose={(choice) => models.choose(agent.applicationId, choice, options.own)}
        />
      ) : (
        <Value>{options.pinned ? t('workspace.settings.modelPinned', '{{model}} (pinned)', { model: options.own ?? '' }) : (current ?? t('workspace.model.agentDefault', 'Agent’s model'))}</Value>
      )}
    </Box>
  );
}

export interface ThreadSettingsProps {
  readonly workspaceId: string;
  readonly projectId: number;
  /** A thread that runs here (not the cloud copy, not a pipeline): the folder's settings apply. */
  readonly local: boolean;
  /** "Work in" (a new thread of a repository's top folder) and where the thread runs. */
  readonly workspace: ReactNode;
  readonly agents: readonly ModelAgent[];
  readonly models: AgentModels;
  /** Offered at all: the cloud copy's own agent answers, with its own model. */
  readonly modelsOffered: boolean;
  /** The language servers, the index and the context ring. */
  readonly meters: ReactNode;
  readonly onStepsError: (message: string) => void;
}

export function ThreadSettings({ workspaceId, projectId, local, workspace, agents, models, modelsOffered, meters, onStepsError }: ThreadSettingsProps): React.JSX.Element {
  return (
    <Box data-testid="thread-settings" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <SettingRow label={t('workspace.settings.workspace', 'Workspace')} testId="thread-settings-workspace">
        {workspace}
      </SettingRow>
      {local && (
        <SettingRow label={t('workspace.settings.sandbox', 'Sandbox')} testId="thread-settings-sandbox">
          <SandboxValue workspaceId={workspaceId} />
        </SettingRow>
      )}
      {local && (
        <SettingRow label={t('workspace.settings.subAgents', 'Sub-agents')} testId="thread-settings-sub-agents">
          <Value>{t('workspace.settings.subAgentsValue', 'Read-only, offered')}</Value>
        </SettingRow>
      )}
      {local && (
        <SettingRow label={t('workspace.stepsChip.menu', 'Step limit')} testId="thread-settings-steps">
          <ComposerPicker label={t('workspace.stepsChip.menu', 'Step limit')} value={<StepLimitValue workspaceId={workspaceId} />} testId="workspace-composer-more">
            {(close) => <StepLimitMenu workspaceId={workspaceId} onError={onStepsError} close={close} />}
          </ComposerPicker>
        </SettingRow>
      )}
      {modelsOffered && agents.length > 0 && (
        <SettingRow label={t('workspace.settings.models', 'Models')} testId="thread-settings-models">
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25, minWidth: 0 }}>
            {agents.map((agent) => (
              <AgentModelRow key={`${String(agent.applicationId)}:${agent.cloud ? 'cloud' : 'local'}`} agent={agent} models={models} workspaceId={workspaceId} projectId={projectId} />
            ))}
          </Box>
        </SettingRow>
      )}
      <SettingRow label={t('workspace.settings.context', 'Context')} testId="thread-settings-context">
        {meters}
      </SettingRow>
    </Box>
  );
}
