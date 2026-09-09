/**
 * The agent editor's tab strip: Configuration, Evaluation, then History.
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
import { RunHistoryPanel } from '@/entities/run-history';
import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

const EDITOR_TABS = {
  configuration: 'configuration',
  evaluation: 'evaluation',
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
}

export function EditApplicationEditorTabs(props: EditApplicationEditorTabsProps): ReactNode {
  const { projectId, applicationId, applicationVersionId, configurationPanel } = props;
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
        <BaseTab
          value={EDITOR_TABS.evaluation}
          label={t('pages.agents.editApplication.tabs.evaluation', 'Evaluation')}
          data-testid="edit-application-tab-evaluation"
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
      <Box hidden={tab !== EDITOR_TABS.evaluation}>
        {tab === EDITOR_TABS.evaluation && (
          <EvaluationPanel
            projectId={projectId}
            applicationId={applicationId}
            applicationVersionId={applicationVersionId}
          />
        )}
      </Box>
      <Box hidden={tab !== EDITOR_TABS.history} sx={historyPanelSx}>
        {tab === EDITOR_TABS.history && (
          <RunHistoryPanel
            projectId={projectId}
            entityName="application"
            entityId={applicationId}
            onClose={() => setTab(EDITOR_TABS.configuration)}
          />
        )}
      </Box>
    </Box>
  );
}
