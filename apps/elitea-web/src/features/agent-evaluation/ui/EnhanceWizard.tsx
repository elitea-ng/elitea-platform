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

import type { EvalEnhanceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { AppliedFix } from '../api/evaluationInsightsApi';
import { evaluationErrorSentence } from '../lib/evaluationError';
import { useRequestPublishSlot } from '../model/requestPublishSlot';
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

export interface EnhanceWithAiProps {
  readonly projectId: string | undefined;
  readonly runId: string;
}

/**
 * The entry point: the button, and the wizard it opens.
 *
 * THE PAID CALL IS FIRED BY THE CLICK, not by an effect in the wizard. An
 * effect that asks on mount runs twice under StrictMode (and on any remount),
 * and each ask is a model call that is billed.
 */
export function EnhanceWithAi({ projectId, runId }: EnhanceWithAiProps): ReactNode {
  const enhance = useEnhanceRun(projectId);
  // Counted per opening: the wizard is remounted each time, so a second run never starts from the first one's review.
  const [opening, setOpening] = useState(0);
  const [open, setOpen] = useState(false);
  const { mutate, reset } = enhance;
  const start = (): void => {
    reset();
    mutate(runId);
    setOpening((count) => count + 1);
    setOpen(true);
  };
  return (
    <>
      <BaseBtn variant="secondary" data-testid="run-enhance" onClick={start}>
        {t('features.agentEvaluation.enhance.open', 'Enhance with AI')}
      </BaseBtn>
      {open && (
        <EnhanceWizardBody key={opening} projectId={projectId} enhance={enhance} onClose={() => setOpen(false)} />
      )}
    </>
  );
}

interface EnhanceWizardBodyProps {
  readonly projectId: string | undefined;
  readonly enhance: ReturnType<typeof useEnhanceRun>;
  readonly onClose: () => void;
}

function fixKey(fix: Pick<AppliedFix, 'kind' | 'id'>): string {
  return `${fix.kind}:${fix.id}`;
}

function EnhanceWizardBody({ projectId, enhance, onClose }: EnhanceWizardBodyProps): ReactNode {
  const apply = useApplyEnhancement(projectId);
  const [step, setStep] = useState<StepKey>('instructions');
  // Every fix's latest outcome, by key. A retry replaces a failure with its new outcome.
  const [outcomes, setOutcomes] = useState<ReadonlyMap<string, AppliedFix>>(new Map());
  const [publishVersionId, setPublishVersionId] = useState<number | undefined>(undefined);

  const proposal = enhance.data;
  const accepted = useAcceptance(proposal);
  const current = useVersionInstructions(projectId, proposal);
  const library = useEvalDimensions(projectId, proposal?.application_id ?? undefined);
  const renderRequestPublish = useRequestPublishSlot();

  // What is still to save: the accepted fixes minus those already saved. A failed one stays,
  // so Save is a retry of exactly the failures and never creates a second draft.
  const pending = useMemo(() => {
    const saved = (fix: Pick<AppliedFix, 'kind' | 'id'>): boolean =>
      outcomes.get(fixKey(fix)) !== undefined && outcomes.get(fixKey(fix))?.error === undefined;
    return {
      instructions:
        accepted.instructions && !!proposal?.instruction_patch && !saved({ kind: 'instructions', id: 'instructions' }),
      dimensionIds: new Set([...accepted.dimensionIds].filter((id) => !saved({ kind: 'dimension', id }))),
      caseIds: new Set([...accepted.caseIds].filter((id) => !saved({ kind: 'case', id }))),
    };
  }, [accepted.instructions, accepted.dimensionIds, accepted.caseIds, proposal, outcomes]);
  const counts = {
    instructions: pending.instructions ? 1 : 0,
    dimensions: pending.dimensionIds.size,
    cases: pending.caseIds.size,
  };
  const total = counts.instructions + counts.dimensions + counts.cases;
  const stepIndex = STEP_KEYS.indexOf(step);
  const applied = outcomes.size === 0 ? undefined : [...outcomes.values()];

  const save = (): void => {
    if (proposal === undefined || total === 0) return;
    apply.mutate(
      { proposal, accepted: pending, library: library.data ?? [] },
      {
        onSuccess: (fixes) =>
          setOutcomes((previous) => new Map([...previous, ...fixes.map((fix) => [fixKey(fix), fix] as const)])),
      },
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
              canSave={total > 0 && !apply.isPending}
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
            review={{
              counts,
              applied,
              applying: apply.isPending,
              error: apply.error,
              onRequestPublish: renderRequestPublish === undefined ? undefined : setPublishVersionId,
            }}
          />
        }
      />
      {publishVersionId !== undefined &&
        renderRequestPublish?.({
          applicationId: proposal?.application_id ?? undefined,
          versionId: publishVersionId,
          onClose: () => setPublishVersionId(undefined),
        })}
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

function WizardContent({
  pending,
  error,
  proposal,
  step,
  accepted,
  currentInstructions,
  review,
}: WizardContentProps): ReactNode {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {pending && (
        <Box data-testid="enhance-loading">
          <CircularProgress aria-label={t('features.agentEvaluation.enhance.analysing', 'Analysing the run')} />
          <Typography variant="bodySmall" color="text.secondary">
            {t(
              'features.agentEvaluation.enhance.analysingNote',
              'Reading the agent version and the cases that missed their targets…',
            )}
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
          <StepBody
            step={step}
            proposal={proposal}
            accepted={accepted}
            currentInstructions={currentInstructions}
            review={review}
          />
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
      return (
        <InstructionStep
          proposal={proposal}
          current={currentInstructions}
          accepted={accepted.instructions}
          onChange={accepted.setInstructions}
        />
      );
    case 'dimensions':
      return (
        <DimensionStep
          changes={proposal.dimension_changes}
          accepted={accepted.dimensionIds}
          onToggle={accepted.toggleDimension}
        />
      );
    case 'cases':
      return <CaseStep changes={proposal.dataset_changes} accepted={accepted.caseIds} onToggle={accepted.toggleCase} />;
    case 'review':
      return <EnhanceReview {...review} />;
  }
}
