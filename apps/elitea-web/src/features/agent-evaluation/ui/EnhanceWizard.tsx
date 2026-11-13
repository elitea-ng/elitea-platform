/**
 * Enhance with AI (issue 2009): a stepped review of what the model proposes
 * after a run missed its targets.
 *
 * Opening the wizard makes ONE proposal request. The model reads the run's
 * pinned agent version and the cases that missed; the answer is three lists
 * (instructions, rubrics and targets, expected outputs), reviewed one step at
 * a time. Nothing is saved until the last step, and each accepted fix is saved
 * by the route that owns it, so permissions and the agent's release path are
 * unchanged.
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Step from '@mui/material/Step';
import StepLabel from '@mui/material/StepLabel';
import Stepper from '@mui/material/Stepper';
import Typography from '@mui/material/Typography';

import { RequestPublishDialog, useAgentRegistry } from '@/features/agent-lifecycle';
import type { EvalEnhanceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { AppliedFix } from '../api/evaluationInsightsApi';
import { evaluationErrorSentence } from '../lib/evaluationError';
import { useEvalDimensions } from '../model/useEvalDimensions';
import { useApplyEnhancement, useEnhanceRun, useVersionInstructions } from '../model/useEnhance';
import { EnhanceReview, type EnhanceReviewProps } from './EnhanceReview';
import { CaseStep, DimensionStep, InstructionStep } from './EnhanceSteps';

const STEP_KEYS = ['instructions', 'dimensions', 'cases', 'review'] as const;
type StepKey = (typeof STEP_KEYS)[number];

function stepLabel(step: StepKey): string {
  switch (step) {
    case 'instructions':
      return t('features.agentEvaluation.enhance.stepInstructions', 'Instructions');
    case 'dimensions':
      return t('features.agentEvaluation.enhance.stepDimensions', 'Rubrics and targets');
    case 'cases':
      return t('features.agentEvaluation.enhance.stepCases', 'Expected outputs');
    case 'review':
      return t('features.agentEvaluation.enhance.stepReview', 'Review and save');
  }
}

/** Everything is accepted by default: the reviewer unticks what they do not want. */
function useAcceptance(proposal: EvalEnhanceProposal | undefined) {
  const [instructions, setInstructions] = useState(true);
  const [dimensionIds, setDimensionIds] = useState<ReadonlySet<string>>(new Set());
  const [caseIds, setCaseIds] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => {
    if (proposal === undefined) return;
    setDimensionIds(new Set(proposal.dimension_changes.map((change) => change.dimension_id)));
    setCaseIds(new Set(proposal.dataset_changes.map((change) => change.case_id)));
  }, [proposal]);
  const toggle = (set: ReadonlySet<string>, id: string, next: boolean): ReadonlySet<string> => {
    const copy = new Set(set);
    if (next) copy.add(id);
    else copy.delete(id);
    return copy;
  };
  return {
    instructions,
    dimensionIds,
    caseIds,
    setInstructions,
    toggleDimension: (id: string, next: boolean) => setDimensionIds((current) => toggle(current, id, next)),
    toggleCase: (id: string, next: boolean) => setCaseIds((current) => toggle(current, id, next)),
  };
}

export interface EnhanceWizardProps {
  readonly projectId: string | undefined;
  /** `undefined` keeps the wizard closed. */
  readonly runId: string | undefined;
  readonly onClose: () => void;
}

export function EnhanceWizard({ projectId, runId, onClose }: EnhanceWizardProps): ReactNode {
  // Remounted per opening, so a second run never starts from the first one's review.
  return runId === undefined ? null : <EnhanceWizardBody projectId={projectId} runId={runId} onClose={onClose} />;
}

function EnhanceWizardBody({ projectId, runId, onClose }: EnhanceWizardProps & { readonly runId: string }): ReactNode {
  const enhance = useEnhanceRun(projectId);
  const apply = useApplyEnhancement(projectId);
  const [step, setStep] = useState<StepKey>('instructions');
  const [applied, setApplied] = useState<readonly AppliedFix[] | undefined>(undefined);
  const [publishVersionId, setPublishVersionId] = useState<number | undefined>(undefined);

  // One request per opening. `mutate` is stable; the guard keeps a re-render from asking twice.
  const { mutate, isIdle } = enhance;
  useEffect(() => {
    if (isIdle) mutate(runId);
  }, [isIdle, mutate, runId]);

  const proposal = enhance.data;
  const accepted = useAcceptance(proposal);
  const current = useVersionInstructions(projectId, proposal);
  const library = useEvalDimensions(projectId, proposal?.application_id ?? undefined);
  const registry = useAgentRegistry(projectId, proposal?.application_id ?? undefined);
  const counts = useMemo(
    () => ({
      instructions: accepted.instructions && proposal?.instruction_patch ? 1 : 0,
      dimensions: accepted.dimensionIds.size,
      cases: accepted.caseIds.size,
    }),
    [accepted.instructions, accepted.dimensionIds, accepted.caseIds, proposal],
  );
  const total = counts.instructions + counts.dimensions + counts.cases;
  const stepIndex = STEP_KEYS.indexOf(step);

  const save = (): void => {
    if (proposal === undefined || total === 0) return;
    apply.mutate(
      {
        proposal,
        accepted: { instructions: accepted.instructions, dimensionIds: accepted.dimensionIds, caseIds: accepted.caseIds },
        library: library.data ?? [],
      },
      { onSuccess: setApplied },
    );
  };

  return (
    <>
      <BaseModal
        open
        variant="complex"
        data-testid="enhance-wizard"
        title={t('features.agentEvaluation.enhance.title', 'Enhance with AI')}
        onClose={onClose}
        actions={{
          node: (
            <WizardActions
              step={step}
              canSave={total > 0 && applied === undefined && !apply.isPending}
              ready={proposal !== undefined}
              onBack={() => setStep(STEP_KEYS[Math.max(0, stepIndex - 1)] ?? 'instructions')}
              onNext={() => setStep(STEP_KEYS[Math.min(STEP_KEYS.length - 1, stepIndex + 1)] ?? 'review')}
              onSave={save}
              onClose={onClose}
            />
          ),
        }}
        content={
          <WizardContent
            pending={enhance.isPending}
            error={enhance.isError ? enhance.error : undefined}
            proposal={proposal}
            step={step}
            accepted={accepted}
            currentInstructions={current.data}
            review={{ counts, applied, applying: apply.isPending, error: apply.error, onRequestPublish: setPublishVersionId }}
          />
        }
      />
      <RequestPublishDialog
        open={publishVersionId !== undefined}
        projectId={projectId}
        applicationId={proposal?.application_id ?? undefined}
        versionId={publishVersionId}
        riskTier={registry.data?.risk_tier}
        onClose={() => setPublishVersionId(undefined)}
      />
    </>
  );
}

interface WizardActionsProps {
  readonly step: StepKey;
  readonly canSave: boolean;
  readonly ready: boolean;
  readonly onBack: () => void;
  readonly onNext: () => void;
  readonly onSave: () => void;
  readonly onClose: () => void;
}

function WizardActions({ step, canSave, ready, onBack, onNext, onSave, onClose }: WizardActionsProps): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: '0.5rem', width: '100%', justifyContent: 'flex-end' }}>
      <BaseBtn variant="secondary" onClick={onClose} data-testid="enhance-close">
        {t('features.agentEvaluation.enhance.close', 'Close')}
      </BaseBtn>
      {step !== 'instructions' && (
        <BaseBtn variant="secondary" onClick={onBack} data-testid="enhance-back">
          {t('features.agentEvaluation.enhance.back', 'Back')}
        </BaseBtn>
      )}
      {step === 'review' ? (
        <BaseBtn variant="contained" disabled={!canSave} onClick={onSave} data-testid="enhance-save">
          {t('features.agentEvaluation.enhance.save', 'Save accepted changes')}
        </BaseBtn>
      ) : (
        <BaseBtn variant="contained" disabled={!ready} onClick={onNext} data-testid="enhance-next">
          {t('features.agentEvaluation.enhance.next', 'Next')}
        </BaseBtn>
      )}
    </Box>
  );
}

type Acceptance = ReturnType<typeof useAcceptance>;

interface WizardContentProps {
  readonly pending: boolean;
  readonly error: unknown;
  readonly proposal: EvalEnhanceProposal | undefined;
  readonly step: StepKey;
  readonly accepted: Acceptance;
  readonly currentInstructions: string | undefined;
  readonly review: EnhanceReviewProps;
}

function WizardContent({ pending, error, proposal, step, accepted, currentInstructions, review }: WizardContentProps): ReactNode {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {pending && (
        <Box data-testid="enhance-loading">
          <CircularProgress aria-label={t('features.agentEvaluation.enhance.analysing', 'Analysing the run')} />
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.enhance.analysingNote', 'Reading the agent version and the cases that missed their targets…')}
          </Typography>
        </Box>
      )}
      {error !== undefined && (
        <Alert severity="error" data-testid="enhance-error">
          {evaluationErrorSentence(error)}
        </Alert>
      )}
      {proposal !== undefined && (
        <>
          <Typography variant="bodyMedium" data-testid="enhance-summary">
            {proposal.summary}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.enhance.evidence', 'Based on {{analysed}} of {{missed}} missed result(s).', {
              analysed: proposal.analysed_rows,
              missed: proposal.missed_rows,
            })}
          </Typography>
          <Stepper activeStep={STEP_KEYS.indexOf(step)}>
            {STEP_KEYS.map((key) => (
              <Step key={key}>
                <StepLabel>{stepLabel(key)}</StepLabel>
              </Step>
            ))}
          </Stepper>
          <StepBody step={step} proposal={proposal} accepted={accepted} currentInstructions={currentInstructions} review={review} />
        </>
      )}
    </Box>
  );
}

interface StepBodyProps {
  readonly step: StepKey;
  readonly proposal: EvalEnhanceProposal;
  readonly accepted: Acceptance;
  readonly currentInstructions: string | undefined;
  readonly review: EnhanceReviewProps;
}

function StepBody({ step, proposal, accepted, currentInstructions, review }: StepBodyProps): ReactNode {
  switch (step) {
    case 'instructions':
      return <InstructionStep proposal={proposal} current={currentInstructions} accepted={accepted.instructions} onChange={accepted.setInstructions} />;
    case 'dimensions':
      return <DimensionStep changes={proposal.dimension_changes} accepted={accepted.dimensionIds} onToggle={accepted.toggleDimension} />;
    case 'cases':
      return <CaseStep changes={proposal.dataset_changes} accepted={accepted.caseIds} onToggle={accepted.toggleCase} />;
    case 'review':
      return <EnhanceReview {...review} />;
  }
}
