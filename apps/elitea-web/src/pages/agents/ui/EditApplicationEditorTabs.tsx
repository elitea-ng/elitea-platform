/**
 * The agent editor's tab strip: Configuration, then Evaluation.
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
 */
import { useState, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { EvaluationPanel } from '@/features/agent-evaluation';
import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

const EDITOR_TABS = {
  configuration: 'configuration',
  evaluation: 'evaluation',
} as const;
type EditorTab = (typeof EDITOR_TABS)[keyof typeof EDITOR_TABS];

const stripSx: SxProps<Theme> = {
  borderBottom: 1,
  borderColor: 'divider',
  marginBottom: '1rem',
};

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
      </BaseTabs>

      {/*
        Both panels stay MOUNTED and one is hidden, rather than one being
        unmounted. The configuration panel holds unsaved edits, and unmounting
        it on a tab switch would discard them silently — the same class of loss
        the unsaved-changes nav blocker (#133) exists to prevent, arriving
        through a control that looks like it only changes what is on screen.
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
    </Box>
  );
}
