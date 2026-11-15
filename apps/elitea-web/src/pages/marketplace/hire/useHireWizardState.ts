/**
 * The hire wizard's state: which step it is on, what was chosen, the hire call
 * and its result. Every open starts again, so a closed wizard never resumes
 * with a half-made choice.
 */
import { useEffect, useState } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { hireMarketplaceListing } from '@/shared/api/generated/marketplace/marketplace';
import type {
  MarketplaceHireResult,
  MarketplaceListingDetail,
  MarketplaceRequirements,
  MarketplaceVersion,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

import { invalidateMarketplace } from '../invalidate';

import {
  DISABLED_VALUE,
  bindingsOf,
  defaultVersionId,
  isHireable,
  unboundRequirements,
  type BindingChoices,
  type HireMode,
} from './hireModel';

export const STEPS = ['version', 'mode', 'bindings', 'review'] as const;
export type Step = (typeof STEPS)[number];

const NO_REQUIREMENTS: MarketplaceRequirements = { items: [], model_class: '', tier: 'low', has_write_tools: false };

export interface HireWizardState {
  readonly step: Step;
  readonly index: number;
  readonly versionId: string;
  readonly version: MarketplaceVersion | undefined;
  readonly requirements: MarketplaceRequirements;
  readonly mode: HireMode;
  readonly choices: BindingChoices;
  readonly result: MarketplaceHireResult | undefined;
  readonly canNext: boolean;
  readonly hire: {
    readonly isPending: boolean;
    readonly isError: boolean;
    readonly error: unknown;
    readonly run: () => void;
  };
  readonly setVersionId: (id: string) => void;
  readonly setMode: (mode: HireMode) => void;
  readonly choose: (key: string, value: string) => void;
  readonly go: (offset: 1 | -1) => void;
}

export function useHireWizardState(
  open: boolean,
  projectId: string | undefined,
  detail: MarketplaceListingDetail,
  initialMode: HireMode,
): HireWizardState {
  const queryClient = useQueryClient();
  const [step, setStep] = useState<Step>('version');
  const [versionId, setVersionId] = useState('');
  const [mode, setMode] = useState<HireMode>(initialMode);
  const [choices, setChoices] = useState<BindingChoices>({});
  const [result, setResult] = useState<MarketplaceHireResult>();

  useEffect(() => {
    if (!open) return;
    setStep('version');
    setVersionId(defaultVersionId(detail.versions));
    setMode(initialMode);
    setChoices({});
    setResult(undefined);
  }, [open, detail.versions, initialMode]);

  const version = detail.versions.find((candidate) => candidate.id === versionId);
  const requirements = version?.requirements ?? NO_REQUIREMENTS;
  const hire = useMutation({
    mutationFn: async (): Promise<MarketplaceHireResult> => {
      const response = await hireMarketplaceListing(projectId ?? '', {
        listing_id: detail.listing.id,
        version_id: versionId,
        mode,
        bindings: bindingsOf(requirements, choices),
      });
      return unwrapBody(response) as MarketplaceHireResult;
    },
    onSuccess: (created) => {
      setResult(created);
      invalidateMarketplace(queryClient);
    },
  });

  const index = STEPS.indexOf(step);
  const versionReady = version !== undefined && isHireable(version);
  const canNext =
    (step !== 'version' || versionReady) &&
    (step !== 'bindings' || unboundRequirements(requirements, choices).length === 0);

  const choose = (key: string, value: string): void => {
    setChoices((current) => {
      const next = { ...current };
      if (value === '') delete next[key];
      else next[key] = value === DISABLED_VALUE ? { kind: 'disabled' } : { kind: 'entity', id: value };
      return next;
    });
  };

  return {
    step,
    index,
    versionId,
    version,
    requirements,
    mode,
    choices,
    result,
    canNext,
    hire: { isPending: hire.isPending, isError: hire.isError, error: hire.error, run: () => hire.mutate() },
    setVersionId,
    setMode,
    choose,
    go: (offset) => setStep(STEPS[index + offset] ?? step),
  };
}
