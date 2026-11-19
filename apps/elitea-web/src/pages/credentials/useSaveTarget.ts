/**
 * pages/credentials/useSaveTarget.ts — which stored credential the form's saves write, once
 * the Microsoft sign-in has saved one from the create screen.
 *
 * The sign-in needs a saved credential (the server starts a sign-in for a saved one, and reads
 * its client, authority and scopes from the row), so on the create screen its button saves
 * first and stays on the form. From then on the form is editing that credential: the next
 * Save updates it instead of creating a second one. Split out of the controller to keep its
 * complexity in budget.
 */
import { useCallback, useMemo, useState } from 'react';

import type { SaveMode, SaveOutcome } from './credentialSave';

interface CreatedCredential {
  readonly id: string;
  readonly uuid: string;
}

interface SaveTargetMode extends SaveMode {
  readonly configId?: string | undefined;
}

interface DetailIdentity {
  readonly uuid?: unknown;
  readonly uid?: unknown;
}

function uuidOf(detail: DetailIdentity | undefined, created: CreatedCredential | undefined): string | undefined {
  const stored = detail?.uuid ?? detail?.uid;
  return typeof stored === 'string' && stored !== '' ? stored : created?.uuid;
}

export interface SaveTarget {
  /** What a save writes: the form's own mode, or the credential the sign-in created. */
  readonly saveMode: SaveMode;
  /** The saved credential's uuid, or `undefined` while nothing is saved. */
  readonly credentialUuid: string | undefined;
  /** Whether the sign-in has already created the credential on the create screen. */
  readonly created: boolean;
  readonly remember: (created: CreatedCredential) => void;
}

export function useSaveTarget(mode: SaveTargetMode, detail: DetailIdentity | undefined): SaveTarget {
  const [created, setCreated] = useState<CreatedCredential | undefined>(undefined);
  const saveMode = useMemo<SaveMode>(() => (created === undefined ? mode : { kind: 'edit', configId: created.id }), [created, mode]);
  return { saveMode, credentialUuid: uuidOf(detail, created), created: created !== undefined, remember: setCreated };
}

/** The save the sign-in runs first: the form's own save, then the credential's uuid. A create-screen save is remembered, so the next Save updates it. */
export function useSaveForSignIn(
  runSave: () => Promise<SaveOutcome | undefined>,
  target: SaveTarget,
  mode: SaveTargetMode,
): () => Promise<string | undefined> {
  const { credentialUuid, created, remember } = target;
  return useCallback(async (): Promise<string | undefined> => {
    const outcome = await runSave();
    if (outcome?.status !== 'ok') return undefined;
    const uuid = outcome.uuid ?? credentialUuid;
    if (mode.kind === 'create' && !created && outcome.id !== undefined && uuid !== undefined) remember({ id: outcome.id, uuid });
    return uuid;
  }, [runSave, credentialUuid, mode.kind, created, remember]);
}
