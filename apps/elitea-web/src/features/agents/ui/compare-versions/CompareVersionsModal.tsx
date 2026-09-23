import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import MenuItem from '@mui/material/MenuItem';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { useVersionDetail } from '../../api/versionComparison';
import { AGENT_COMPARE_STEPS, extractAgentCompareData, sortVersionsNewestFirst } from '../../lib/compareVersions';
import type { AgentCompareData } from '../../lib/compareVersions';
import type { AgentPipelineVersionOption } from '../../lib/types';
import { CompareInstructionsStep, CompareToolsSkillsStep, CompareUserInteractionStep } from './CompareVersionsSteps';

/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/compare-versions/ui/
 * CompareVersionsModal.jsx` — pick a second version, then walk the three
 * comparison steps.
 *
 * **DISCLOSED DEVIATIONS.**
 *  - Read-only. See `./CompareVersionsSteps.tsx`'s doc comment for why the
 *    baseline's per-field save-back is not ported, and what it would need.
 *    With no edits to lose, the baseline's discard-unsaved-changes
 *    confirmation has nothing to guard and is not ported either.
 *  - The wizard's step indicator + Previous/Next footer becomes a `Tabs`
 *    row. The baseline's own `CompareVersionsStepIndicator` is clickable
 *    (`onStepChange`), so the steps were never a linear wizard in the first
 *    place — the footer buttons were a second way to do what clicking a step
 *    already did. Tabs is this app's existing primitive for exactly that.
 *  - `SingleSelect` with avatars/descriptions becomes a plain `TextField
 *    select`: `shared/ui` has no avatar-bearing select, and the description
 *    the baseline renders (`formatVersionMeta`) is the version's author and
 *    date, which this app's version list (`AgentPipelineVersionOption`) does
 *    not carry beyond `created_at` — so the PICKER (before "Compare" is
 *    clicked) still cannot show it. The comparison view can (elitea_issues
 *    #6563): both sides' full `ApplicationVersionDetail` are fetched by then
 *    and already carry `author`, appended onto the same version-name label
 *    every pane header renders.
 *
 * The LEFT side is always the version the editor currently has open; only
 * the right side is chosen, matching the baseline's `leftVersionId` prop and
 * its `availableVersions` filter.
 */
/**
 * elitea_issues: #6563 — appends the version's author, when known, onto the
 * plain name label every pane header already renders. Pulled out of the
 * component so the two call sites (left/right) do not each add a branch to
 * `CompareVersionsModal`'s own complexity budget.
 */
function versionLabelWithAuthor(name: string, detail: ApplicationVersionDetail | undefined): string {
  const authorName = detail?.author?.name;
  return authorName === undefined
    ? name
    : t('features.agents.compareVersions.versionByAuthor', '{{name}} · {{author}}', { name, author: authorName });
}

export interface CompareVersionsModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly applicationId: number;
  readonly versions: readonly AgentPipelineVersionOption[];
  readonly leftVersionId: number | undefined;
}

/** The version picker. Its own component so `CompareVersionsModal` stays inside the §3.5 complexity budget. */
function VersionSelection(props: {
  leftName: string;
  rightVersionId: number | undefined;
  availableVersions: readonly AgentPipelineVersionOption[];
  onChange: (versionId: number) => void;
}): ReactNode {
  const { leftName, rightVersionId, availableVersions, onChange } = props;
  return (
    <Box sx={selectionSx}>
      <Box>
        <Typography variant="labelMedium">{t('features.agents.compareVersions.baseVersion', 'Base version')}</Typography>
        <Typography color="text.secondary">{leftName}</Typography>
      </Box>
      <TextField
        select
        fullWidth
        label={t('features.agents.compareVersions.compareWith', 'Compare with')}
        value={rightVersionId ?? ''}
        onChange={(event) => onChange(Number(event.target.value))}
        slotProps={{ htmlInput: { 'data-testid': 'compare-versions-select' } }}
      >
        {availableVersions.map((version) => (
          <MenuItem
            key={version.id}
            value={version.id}
          >
            {version.name}
          </MenuItem>
        ))}
      </TextField>
    </Box>
  );
}

/** The three comparison sections behind a tab row — see the modal's doc comment for why tabs, not a wizard footer. */
function ComparisonWizard(props: {
  activeStep: number;
  onStepChange: (step: number) => void;
  leftVersionName: string;
  rightVersionName: string;
  left: AgentCompareData;
  right: AgentCompareData;
}): ReactNode {
  const { activeStep, onStepChange, ...stepProps } = props;
  const activeKey = AGENT_COMPARE_STEPS[activeStep]?.key ?? 'instructions';
  return (
    <Box sx={wizardSx}>
      <Tabs
        value={activeStep}
        onChange={(_event, value: number) => onStepChange(value)}
        aria-label={t('features.agents.compareVersions.stepsAria', 'Comparison sections')}
      >
        {AGENT_COMPARE_STEPS.map((step) => (
          <Tab
            key={step.key}
            label={step.label}
          />
        ))}
      </Tabs>
      {activeKey === 'instructions' && <CompareInstructionsStep {...stepProps} />}
      {activeKey === 'user-interaction' && <CompareUserInteractionStep {...stepProps} />}
      {activeKey === 'tools-skills' && <CompareToolsSkillsStep {...stepProps} />}
    </Box>
  );
}

/** The modal's footer buttons in both phases. Extracted for the same complexity-budget reason as the panes above. */
function CompareActions(props: {
  comparing: boolean;
  canCompare: boolean;
  onCompare: () => void;
  onChangeVersions: () => void;
  onClose: () => void;
}): ReactNode {
  const { comparing, canCompare, onCompare, onChangeVersions, onClose } = props;
  if (comparing) {
    return (
      <>
        <BaseBtn
          variant="secondary"
          size="small"
          onClick={onChangeVersions}
        >
          {t('features.agents.compareVersions.changeVersions', 'Change versions')}
        </BaseBtn>
        <BaseBtn
          variant="elitea"
          size="small"
          onClick={onClose}
        >
          {t('agents.compareVersions.close', 'Close')}
        </BaseBtn>
      </>
    );
  }
  return (
    <>
      <BaseBtn
        variant="secondary"
        size="small"
        onClick={onClose}
      >
        {t('common.cancel', 'Cancel')}
      </BaseBtn>
      <BaseBtn
        variant="elitea"
        size="small"
        disabled={!canCompare}
        onClick={onCompare}
      >
        {t('features.agents.compareVersions.compare', 'Compare')}
      </BaseBtn>
    </>
  );
}

function LoadingPane(): ReactNode {
  return (
    <Box sx={loadingSx}>
      <CircularProgress size={24} />
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {t('features.agents.compareVersions.loading', 'Loading version details…')}
      </Typography>
    </Box>
  );
}

export function CompareVersionsModal(props: CompareVersionsModalProps): ReactNode {
  const { open, onClose, projectId, applicationId, versions, leftVersionId } = props;

  const availableVersions = useMemo(
    () => sortVersionsNewestFirst(versions.filter((version) => version.id !== leftVersionId)),
    [versions, leftVersionId],
  );

  const [rightVersionId, setRightVersionId] = useState<number | undefined>(undefined);
  const [comparing, setComparing] = useState(false);
  const [activeStep, setActiveStep] = useState(0);

  // Reopening starts from the selection step with the newest other version
  // preselected — the baseline's own on-open effect.
  useEffect(() => {
    if (!open) return;
    setRightVersionId(availableVersions[0]?.id);
    setComparing(false);
    setActiveStep(0);
    // `availableVersions` is derived and would re-run this on every render of
    // the parent, resetting a selection the user had just made.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const leftQuery = useVersionDetail({ projectId, applicationId, versionId: leftVersionId, enabled: open && comparing });
  const rightQuery = useVersionDetail({ projectId, applicationId, versionId: rightVersionId, enabled: open && comparing });

  const leftName = versions.find((version) => version.id === leftVersionId)?.name ?? '';
  const rightName = versions.find((version) => version.id === rightVersionId)?.name ?? '';

  /*
   * elitea_issues: #6563 — the version's creator is already on the wire
   * (`ApplicationVersionDetail.author`, the same {id, email, name} object
   * `useVersionDetail` fetches for every other field this modal compares)
   * but nothing rendered it, unlike the baseline's `formatVersionMeta`
   * (see this file's own DISCLOSED DEVIATIONS note above, which only covers
   * the PICKER's list-level name — the comparison view has the real
   * per-version detail loaded and can show it). Appended to the same label
   * both `VersionSelection`'s pane headers and every `ComparisonRow` already
   * render, so no new render slot is needed.
   */
  const leftVersionLabel = versionLabelWithAuthor(leftName, leftQuery.data);
  const rightVersionLabel = versionLabelWithAuthor(rightName, rightQuery.data);

  const left = useMemo(() => extractAgentCompareData(leftQuery.data), [leftQuery.data]);
  const right = useMemo(() => extractAgentCompareData(rightQuery.data), [rightQuery.data]);

  const content = !comparing ? (
    <VersionSelection
      leftName={leftName}
      rightVersionId={rightVersionId}
      availableVersions={availableVersions}
      onChange={setRightVersionId}
    />
  ) : leftQuery.isError || rightQuery.isError ? (
    <Alert severity="error">
      {t('features.agents.compareVersions.loadFailed', 'Failed to load version details. Please try again.')}
    </Alert>
  ) : leftQuery.isPending || rightQuery.isPending ? (
    <LoadingPane />
  ) : (
    <ComparisonWizard
      activeStep={activeStep}
      onStepChange={setActiveStep}
      leftVersionName={leftVersionLabel}
      rightVersionName={rightVersionLabel}
      left={left}
      right={right}
    />
  );

  const actions = (
    <CompareActions
      comparing={comparing}
      canCompare={rightVersionId !== undefined}
      onCompare={() => setComparing(true)}
      onChangeVersions={() => setComparing(false)}
      onClose={onClose}
    />
  );

  return (
    <BaseModal
      open={open}
      variant="complex"
      /* NOT `fullscreen`: `BaseModal` suppresses its whole action bar in
         fullscreen (`hasActions && !isFullscreen`), which would take away
         "Change versions"/"Close" exactly where they are needed. The wizard
         pane widens itself instead. */
      title={t('features.agents.compareVersions.title', 'Compare versions')}
      onClose={onClose}
      content={content}
      actions={{ node: actions }}
      data-testid="compare-versions-modal"
    />
  );
}

const selectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem', minWidth: '28rem' };
const wizardSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '40rem' };
const loadingSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: '0.75rem',
  minHeight: '12rem',
  justifyContent: 'center',
};
