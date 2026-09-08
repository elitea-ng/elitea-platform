/**
 * The adopt-models dialog's state machine: what the provider offers, what is
 * already published, what the operator picked, and what happened when they
 * pressed Adopt.
 *
 * Its own hook so `./LlmProviderAdoptModelsDialog.tsx` is markup — the split
 * `useAdminAppRequestsPage.ts` makes for its own page, and the one the §3.5
 * complexity budget forces on a dialog with two queries, a mutation loop and
 * four pieces of local state.
 */
import { useEffect, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';

import type { AdoptFailure } from './LlmProviderAdoptModelsParts';
import { adoptedModelIDs } from './LlmProviderAdoptModelsParts';
import { configFailureReason } from './api/adminConfigurationApi';
import {
  useAdminPlatformModels,
  useCreatePlatformModel,
  type PlatformModelDraft,
} from './api/adminLlmPlatformModelsApi';
import { useAdminLlmProviderModels } from './api/adminLlmProviderModelsApi';
import type { LlmProvider } from './api/adminLlmProvidersApi';

/**
 * The kind offered when the server has not said which kinds it dispatches.
 *
 * The server's own `model_types` is the authority — a hardcoded client list
 * drifts from the registry the moment a snapshot changes — and this is only
 * what the field shows before that listing has answered.
 */
const FALLBACK_MODEL_TYPES: readonly string[] = ['llm_model'];

/** Stable empties, so a pending query does not make every memo recompute. */
const NO_IDS: readonly string[] = [];

/** Everything the dialog renders, and the three things it can do. */
export interface AdoptProviderModelsState {
  /** The provider's ids, in the provider's own order. */
  readonly models: readonly string[];
  /** The ids this platform already publishes — checked and disabled, never hidden. */
  readonly adopted: ReadonlySet<string>;
  /** The ids that are NOT already published: what "select all" means. */
  readonly selectable: readonly string[];
  readonly selected: readonly string[];
  readonly kind: string;
  readonly modelTypes: readonly string[];
  readonly failures: readonly AdoptFailure[];
  readonly adopting: boolean;
  readonly isPending: boolean;
  readonly truncated: boolean;
  /** The server's own sentence for a failed listing, or undefined. */
  readonly loadError: string | undefined;
  readonly setKind: (kind: string) => void;
  readonly setSelected: (ids: readonly string[]) => void;
  readonly toggle: (id: string) => void;
  readonly adopt: () => void;
}

export function useAdoptProviderModels(
  provider: LlmProvider | undefined,
  onClose: () => void,
): AdoptProviderModelsState {
  const open = provider !== undefined;
  const listing = useAdminLlmProviderModels(provider?.id);
  const catalogue = useAdminPlatformModels();
  const createModel = useCreatePlatformModel();

  const [selected, setSelected] = useState<readonly string[]>(NO_IDS);
  const [kind, setKind] = useState(FALLBACK_MODEL_TYPES[0] ?? '');
  const [failures, setFailures] = useState<readonly AdoptFailure[]>([]);
  const [adopting, setAdopting] = useState(false);

  // ONE effect, and it resets on OPEN only. A reset that also ran while the
  // dialog was open would discard the operator's selection every time the
  // catalogue underneath refetched — which it does after every adopted model.
  useEffect(() => {
    if (!open) return;
    setSelected(NO_IDS);
    setFailures([]);
    setAdopting(false);
  }, [open, provider?.id]);

  // Derived from `listing.data`, which is stable between refetches, rather than
  // from a `?? []` fallback that is a fresh array on every render.
  const models = listing.data?.models ?? NO_IDS;
  const adopted = useMemo(
    () => adoptedModelIDs(catalogue.data?.items ?? []),
    [catalogue.data?.items],
  );
  const selectable = useMemo(() => models.filter((id) => !adopted.has(id)), [models, adopted]);

  const adopt = (): void => {
    if (provider === undefined) return;
    setAdopting(true);
    setFailures([]);
    void adoptSelectedModels(selected, {
      kind,
      credential: provider.elitea_title,
      create: (draft) => createModel.mutateAsync(draft),
    }).then((problems) => {
      setAdopting(false);
      setFailures(problems);
      // The dialog stays OPEN when anything failed: the ids that did not adopt
      // are the only record of what to retry, and closing over them would
      // report a partial adoption as a complete one.
      if (problems.length === 0) onClose();
    });
  };

  return {
    models,
    adopted,
    selectable,
    selected,
    kind,
    modelTypes: catalogue.data?.model_types ?? FALLBACK_MODEL_TYPES,
    failures,
    adopting,
    isPending: open && listing.isPending,
    truncated: listing.data?.truncated === true,
    loadError: listingFailure(listing.error),
    setKind,
    setSelected,
    toggle: (id) => setSelected((current) => toggleID(current, id)),
    adopt,
  };
}

function toggleID(current: readonly string[], id: string): readonly string[] {
  return current.includes(id) ? current.filter((entry) => entry !== id) : [...current, id];
}

/**
 * The server's own sentence for a failed listing, when it gave one.
 *
 * Its refusals name the problem — a rejected credential, an endpoint the
 * allowlist forbids, a type this build cannot list — and a generic "could not
 * read the models" would discard exactly the words that say what to change.
 */
function listingFailure(error: unknown): string | undefined {
  if (error == null) return undefined;
  return (
    configFailureReason(error) ??
    t('pages.admin.adoptModels.loadError', 'Could not read this provider’s models.')
  );
}

/**
 * Creates one platform model per selected id, ONE AT A TIME, and reports the
 * ones that failed.
 *
 * Sequential on purpose. Every create runs provider admission server-side, and
 * a hundred parallel writes would be a burst against the same pool the rest of
 * the admin panel reads through — the bound `check_connections` already applies
 * to its own fan-out, for the same reason.
 *
 * A failure does not stop the run: adopting nine models and being told the
 * tenth already exists is a better outcome than adopting none, and the ids that
 * failed are reported by name so the operator can see which.
 *
 * The write is the SAME one the manual "Add a platform model" dialog makes —
 * `POST /admin/gateway/platform_models` — so the server stays the one place
 * that derives a model's section, validates its credential link and runs
 * admission. Each create invalidates the platform-model listing, which is what
 * keeps the table under this dialog truthful while the adoption runs.
 *
 * The id becomes BOTH the model's wire name and the id callers address. Legacy
 * lower-cased and underscored it ("gpt-4o" became "gpt_4o"), which left the
 * platform advertising a name nobody could guess from the provider's own
 * documentation; the provider's own id is the one thing every caller knows.
 */
async function adoptSelectedModels(
  ids: readonly string[],
  options: {
    readonly kind: string;
    readonly credential: string;
    readonly create: (draft: PlatformModelDraft) => Promise<void>;
  },
): Promise<readonly AdoptFailure[]> {
  const problems: AdoptFailure[] = [];
  for (const id of ids) {
    try {
      await options.create({
        elitea_title: id,
        type: options.kind,
        // The link is what makes this a PLATFORM model. It is required — all
        // five model types declare it, so a model without one fails provider
        // admission and is served by nobody — and the server refuses a link
        // naming anything but a published platform credential.
        data: { name: id, ai_credentials: { elitea_title: options.credential } },
      });
    } catch (error) {
      problems.push({
        id,
        reason:
          configFailureReason(error) ??
          t('pages.admin.adoptModels.unknownFailure', 'the server refused it'),
      });
    }
  }
  return problems;
}
