/**
 * The agent editor's tab strip: Configuration, Persona, Avatar, Evaluation, Memory, Heartbeat, then History.
 *
 * AVATAR (ADR-0050 decision 6, issue 1619) is the version's avatar pack with
 * a preview of every state; like Persona it edits unsaved version state.
 *
 * HEARTBEAT (ADR-0050 decision 2, issue 1618) turns the agent's heartbeat on
 * or off and shows its history — `features/agent-heartbeat`'s `HeartbeatPanel`.
 *
 * MEMORY (ADR-0050 decision 1b, issue 1608) is what the agent remembers in
 * this project — `features/agent-memory`'s `MemoryPanel`; the server decides
 * what the caller sees and may change.
 *
 * The baseline puts Evaluation SECOND on this page
 * (`apps/elitea-ui/src/pages/Applications/EditApplication.jsx:103-113`), and
 * that placement is the whole reason this slice needs no route of its own: an
 * evaluation library is a property of the agent you are editing, and reaching
 * it should not mean leaving the editor.
 *
 * THE EVALUATION SUB-NAVIGATION lives in `features/agent-evaluation`'s own
 * `EvaluationPanel`, not here. It carries Library, Datasets and Runs — the
 * three the reference has a backend for in this release — and NOT the
 * reference's fourth entry, Suite config: there is no suite table, and a
 * sub-tab that renders an empty panel is indistinguishable, to the person
 * looking at it, from a feature that is broken.
 *
 * Extracted from `EditApplication.tsx` to keep that file inside the §3.5
 * 400-line budget.
 *
 * HISTORY (issue #868), AND WHY IT LIVES HERE RATHER THAN INSIDE
 * `features/agents/ui/ConfigurationTab.tsx`. That component declares a
 * `renderRunHistory` slot the issue names, but it is not mounted anywhere in
 * this worktree — `EditApplication.tsx` composes `EditApplicationEditorTabs`
 * + `EditApplicationConfigurationPanel` instead (verified: zero JSX call
 * sites of `<ConfigurationTab>` under `pages/agents`), because its right pane
 * needs an embedded test-chat pane this page does not have
 * (`pages/agents/ui/ChatWithAgentButton.tsx`'s own doc comment: that pane
 * "exists but is still socket-era and mounted by nothing" — a separate,
 * larger, already-disclosed gap than this one). Wiring `renderRunHistory`
 * into a component nothing renders would satisfy the issue's literal file
 * citation while leaving agents' run history exactly as unreachable as
 * before. This tab is the real fix: a third, always-reachable entry in the
 * one tab strip the agent editor page actually mounts, backed by the SAME
 * `RunHistoryPanel` (`@/entities/run-history`) pipelines' and toolkits' own
 * `renderRunHistory` slots now use — the conversation-list + trace view is
 * entity-agnostic, keyed only by `entity_name`/`entity_meta_id`. Agents get
 * no "Restore" action (no live chat pane exists on this page to restore
 * into), same as toolkits.
 */
import { useState, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { EvaluationPanel } from '@/features/agent-evaluation';
import { HeartbeatPanel } from '@/features/agent-heartbeat';
import { MemoryPanel } from '@/features/agent-memory';
import { RunHistoryPanel } from '@/entities/run-history';
import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

const EDITOR_TABS = {
  configuration: 'configuration',
  persona: 'persona',
  avatar: 'avatar',
  evaluation: 'evaluation',
  memory: 'memory',
  heartbeat: 'heartbeat',
  history: 'history',
} as const;
type EditorTab = (typeof EDITOR_TABS)[keyof typeof EDITOR_TABS];

const stripSx: SxProps<Theme> = {
  borderBottom: 1,
  borderColor: 'divider',
  marginBottom: '1rem',
};

const historyPanelSx: SxProps<Theme> = { height: '35rem' };

export interface EditApplicationEditorTabsProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /**
   * The version a run scores. Passed THROUGH rather than resolved inside the
   * evaluation feature: the editor already knows which version is open, and a
   * second resolution here could disagree with the one the Save button writes.
   */
  readonly applicationVersionId: number | undefined;
  /** The existing configuration panel, passed in rather than rebuilt here. */
  readonly configurationPanel: ReactNode;
  /**
   * ADR-0050 — the Persona tab's panel (the version's soul). Passed in, like
   * the configuration panel, because it edits the same unsaved version state
   * the page's Save button writes.
   */
  readonly personaPanel?: ReactNode;
  /** ADR-0050 decision 6 — the Avatar tab's panel (the version's avatar pack), passed in for the same reason. */
  readonly avatarPanel?: ReactNode;
  /** #955 — this agent's own versions, so the History tab can name which one produced each run. */
  readonly versions?: readonly { readonly id: string | number; readonly name: string }[] | undefined;
}

export function EditApplicationEditorTabs(props: EditApplicationEditorTabsProps): ReactNode {
  const { projectId, applicationId, applicationVersionId, configurationPanel, personaPanel, avatarPanel, versions } = props;
  const [tab, setTab] = useState<EditorTab>(EDITOR_TABS.configuration);

  const handleChange = (_event: SyntheticEvent, value: EditorTab): void => setTab(value);

  return (
    <Box data-testid="edit-application-editor-tabs">
      <BaseTabs
        sx={stripSx}
        value={tab}
        onChange={handleChange}
        aria-label={t('pages.agents.editApplication.tabs.label', 'Agent editor sections')}
      >
        <BaseTab
          value={EDITOR_TABS.configuration}
          label={t('pages.agents.editApplication.tabs.configuration', 'Configuration')}
          data-testid="edit-application-tab-configuration"
        />
        {personaPanel !== undefined && (
          <BaseTab
            value={EDITOR_TABS.persona}
            label={t('pages.agents.editApplication.tabs.persona', 'Persona')}
            data-testid="edit-application-tab-persona"
          />
        )}
        {avatarPanel !== undefined && (
          <BaseTab
            value={EDITOR_TABS.avatar}
            label={t('pages.agents.editApplication.tabs.avatar', 'Avatar')}
            data-testid="edit-application-tab-avatar"
          />
        )}
        <BaseTab
          value={EDITOR_TABS.evaluation}
          label={t('pages.agents.editApplication.tabs.evaluation', 'Evaluation')}
          data-testid="edit-application-tab-evaluation"
        />
        <BaseTab
          value={EDITOR_TABS.memory}
          label={t('pages.agents.editApplication.tabs.memory', 'Memory')}
          data-testid="edit-application-tab-memory"
        />
        <BaseTab
          value={EDITOR_TABS.heartbeat}
          label={t('pages.agents.editApplication.tabs.heartbeat', 'Heartbeat')}
          data-testid="edit-application-tab-heartbeat"
        />
        <BaseTab
          value={EDITOR_TABS.history}
          label={t('pages.agents.editApplication.tabs.history', 'History')}
          data-testid="edit-application-tab-history"
        />
      </BaseTabs>

      {/*
        All three panels stay MOUNTED and the rest are hidden, rather than
        unmounted. The configuration panel holds unsaved edits, and unmounting
        it on a tab switch would discard them silently — the same class of loss
        the unsaved-changes nav blocker (#133) exists to prevent, arriving
        through a control that looks like it only changes what is on screen.
        Evaluation and History are cheaper to remount (no local edits to
        lose), so each stays gated on `tab ===` rather than mounted eagerly.
      */}
      <Box hidden={tab !== EDITOR_TABS.configuration}>{configurationPanel}</Box>
      {/* Mounted while hidden for the same reason: it holds unsaved edits. */}
      {personaPanel !== undefined && <Box hidden={tab !== EDITOR_TABS.persona}>{personaPanel}</Box>}
      {avatarPanel !== undefined && <Box hidden={tab !== EDITOR_TABS.avatar}>{avatarPanel}</Box>}
      <Box hidden={tab !== EDITOR_TABS.evaluation}>
        {tab === EDITOR_TABS.evaluation && (
          <EvaluationPanel
            projectId={projectId}
            applicationId={applicationId}
            applicationVersionId={applicationVersionId}
          />
        )}
      </Box>
      <Box hidden={tab !== EDITOR_TABS.memory}>
        {tab === EDITOR_TABS.memory && <MemoryPanel projectId={projectId} applicationId={applicationId} />}
      </Box>
      <Box hidden={tab !== EDITOR_TABS.heartbeat}>
        {tab === EDITOR_TABS.heartbeat && <HeartbeatPanel projectId={projectId} applicationId={applicationId} />}
      </Box>
      <Box hidden={tab !== EDITOR_TABS.history} sx={historyPanelSx}>
        {tab === EDITOR_TABS.history && (
          <RunHistoryPanel
            projectId={projectId}
            entityName="application"
            entityId={applicationId}
            onClose={() => setTab(EDITOR_TABS.configuration)}
            {...(versions !== undefined ? { versions } : {})}
          />
        )}
      </Box>
    </Box>
  );
}
