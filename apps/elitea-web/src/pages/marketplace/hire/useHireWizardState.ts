/**
 * The hire wizard's state: which step it is on, what was chosen, the hire call
 * and its result. Every open starts again, so a closed wizard never resumes
 * with a half-made choice.
 */
import { useEffect, useRef, useState } from 'react';

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
  const [versionId, setVersionIdState] = useState('');
  const [mode, setMode] = useState<HireMode>(initialMode);
  const [choices, setChoices] = useState<BindingChoices>({});
  const [result, setResult] = useState<MarketplaceHireResult>();

  // The wizard starts from what the page held WHEN IT OPENED. A refetch of the listing (the success
  // invalidates it) must not reset the wizard, or the result step would vanish after a hire.
  const latest = useRef({ versions: detail.versions, initialMode });
  useEffect(() => {
    latest.current = { versions: detail.versions, initialMode };
  });
  useEffect(() => {
    if (!open) return;
    setStep('version');
    setVersionIdState(defaultVersionId(latest.current.versions));
    setMode(latest.current.initialMode);
    setChoices({});
    setResult(undefined);
  }, [open]);

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

  // A binding belongs to one version's requirements: a new version starts with none, and with no old error.
  const setVersionId = (id: string): void => {
    setVersionIdState(id);
    setChoices({});
    hire.reset();
  };
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
    go: (offset) => {
      hire.reset();
      setStep(STEPS[index + offset] ?? step);
    },
  };
}
