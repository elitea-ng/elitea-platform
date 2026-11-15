/**
 * The hire wizard (ADR-0051 decision 3): Version, Mode, Bindings, Review.
 *
 * Install is the default: a read-only agent pinned to the version. Fork gives
 * an editable copy that remembers where it came from. Every required toolkit
 * or MCP type is bound to a project toolkit, or marked "Disable for now"; the
 * wizard does not finish while one is left. Above low tier, or with write tools,
 * the agent starts with its tools off and a change request enables them, and
 * the Review step says so before the person presses Hire.
 *
 * The dialog is the same on web and desktop: the desktop theme layer gives it
 * the compact density. The steps are in `HireSteps.tsx`, the state in
 * `useHireWizardState.ts`.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import type { MarketplaceListingDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { BindingsStep, ModeStep, ReviewStep, VersionStep } from './HireSteps';
import { hireErrorMessage, type HireMode } from './hireModel';
import { STEPS, useHireWizardState, type HireWizardState, type Step } from './useHireWizardState';
import { useBindingTargets } from './useBindingTargets';

function stepTitle(step: Step): string {
  switch (step) {
    case 'version':
      return t('marketplace.hire.step.version', 'Version');
    case 'mode':
      return t('marketplace.hire.step.mode', 'Mode');
    case 'bindings':
      return t('marketplace.hire.step.bindings', 'Bindings');
    case 'review':
      return t('marketplace.hire.step.review', 'Review');
  }
}

export interface HireWizardProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly detail: MarketplaceListingDetail;
  readonly initialMode: HireMode;
}

function Actions({
  state,
  onClose,
  onOpenAgent,
}: {
  readonly state: HireWizardState;
  readonly onClose: () => void;
  readonly onOpenAgent: () => void;
}): ReactNode {
  if (state.result !== undefined) {
    return (
      <BaseBtn variant="contained" onClick={onOpenAgent} data-testid="hire-open-agent">
        {t('marketplace.hire.open', 'Open the agent')}
      </BaseBtn>
    );
  }
  return (
    <>
      <BaseBtn variant="secondary" onClick={onClose}>
        {t('shared.ui.baseModal.cancel', 'Cancel')}
      </BaseBtn>
      {state.index > 0 && (
        <BaseBtn variant="secondary" onClick={() => state.go(-1)} data-testid="hire-back">
          {t('marketplace.hire.back', 'Back')}
        </BaseBtn>
      )}
      {state.step === 'review' ? (
        <BaseBtn
          variant="contained"
          disabled={state.hire.isPending || state.version === undefined}
          onClick={state.hire.run}
          data-testid="hire-confirm"
        >
          {t('marketplace.hire.confirm', 'Hire')}
        </BaseBtn>
      ) : (
        <BaseBtn variant="contained" disabled={!state.canNext} onClick={() => state.go(1)} data-testid="hire-next">
          {t('marketplace.hire.next', 'Next')}
        </BaseBtn>
      )}
    </>
  );
}

interface BodyProps {
  readonly state: HireWizardState;
  readonly detail: MarketplaceListingDetail;
  readonly optionsFor: Parameters<typeof BindingsStep>[0]['optionsFor'];
}

function Body({ state, detail, optionsFor }: BodyProps): ReactNode {
  const { step, result } = state;
  if (result !== undefined) {
    return (
      <Alert severity={result.awaiting_approval ? 'info' : 'success'} data-testid="hire-done">
        {result.awaiting_approval
          ? t(
              'marketplace.hire.doneApproval',
              'Hired. The tools stay off until a person approves the hire change request.',
            )
          : t('marketplace.hire.done', 'Hired. The agent is in your Workshop.')}
      </Alert>
    );
  }
  return (
    <>
      <Typography variant="bodySmall" color="text.secondary" aria-live="polite" data-testid="hire-step-label">
        {t('marketplace.hire.stepOf', 'Step {{n}} of {{total}}: {{title}}', {
          n: state.index + 1,
          total: STEPS.length,
          title: stepTitle(step),
        })}
      </Typography>
      {step === 'version' && (
        <VersionStep versions={detail.versions} value={state.versionId} onChange={state.setVersionId} />
      )}
      {step === 'mode' && <ModeStep value={state.mode} onChange={state.setMode} />}
      {step === 'bindings' && (
        <BindingsStep
          requirements={state.requirements}
          choices={state.choices}
          optionsFor={optionsFor}
          onChoose={state.choose}
        />
      )}
      {step === 'review' && <ReviewStep requirements={state.requirements} mode={state.mode} version={state.version} />}
      {state.hire.isError && (
        <Alert severity="error" data-testid="hire-error">
          {hireErrorMessage(state.hire.error)}
        </Alert>
      )}
    </>
  );
}

export function HireWizard({ open, onClose, projectId, detail, initialMode }: HireWizardProps): ReactNode {
  const navigate = useNavigate();
  const state = useHireWizardState(open, projectId, detail, initialMode);
  const targets = useBindingTargets(projectId, open);

  const openAgent = (): void => {
    if (state.result === undefined) return;
    onClose();
    void navigate({ to: '/agents/$tab/$agentId', params: { tab: 'all', agentId: state.result.application_id } });
  };

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('marketplace.hire.title', 'Hire {{name}}', { name: detail.listing.name })}
      data-testid="hire-wizard"
      actions={{ node: <Actions state={state} onClose={onClose} onOpenAgent={openAgent} /> }}
      content={
        <Box sx={contentSx}>
          <Body state={state} detail={detail} optionsFor={targets.optionsFor} />
        </Box>
      }
    />
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: 2, minWidth: { xs: 0, sm: 420 } };
