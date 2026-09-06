/**
 * pages/credentials/CreateCredential.tsx — the `/credentials/create-credential`
 * and `/credentials/create-credential/:credentialType` route targets (also
 * reused, via `mode.configurationMode`, for `/settings/create-configuration`
 * and `/settings/create-configuration/:credentialType`). Ported from
 * `apps/elitea-ui/src/pages/Credentials/CreateCredential.jsx`. Manifest
 * ROUTE-023/024, ROUTE-063/064.
 *
 * Thin route-target wrapper: all real logic lives in `CredentialForm.tsx`.
 * PARAM-037/039/041/043/045 (`forceCustom`/`from`/`prefill_id`/
 * `prefill_name`/`section`) are unit R1's validated search params — this
 * component accepts their resolved values as plain props rather than
 * reading `useSearch()` itself, since it does not own the route file that
 * would supply a typed search schema (see this unit's final report).
 */
import type { ReactNode } from 'react';

import type { CredentialFormContext } from './CredentialForm';
import { CredentialForm } from './CredentialForm';

export interface CreateCredentialProps {
  readonly context: CredentialFormContext;
  readonly credentialType?: string;
  readonly configurationMode?: boolean;
  readonly prefillName?: string;
  readonly prefillId?: string;
  readonly section?: string;
  /** See `useCredentialFormController.ts`'s `onSaved` doc comment. */
  readonly onCreated: (savedId?: string) => void;
  readonly onCancelled: () => void;
  readonly onTypeChosen?: (type: string) => void;
}

export function CreateCredential({
  context,
  credentialType,
  configurationMode = false,
  prefillName,
  prefillId,
  section,
  onCreated,
  onCancelled,
  onTypeChosen,
}: CreateCredentialProps): ReactNode {
  return (
    <CredentialForm
      context={context}
      mode={{ kind: 'create', ...(credentialType !== undefined ? { credentialType } : {}), configurationMode }}
      onSaved={onCreated}
      onDiscarded={onCancelled}
      prefill={{ ...(prefillName !== undefined ? { name: prefillName } : {}), ...(prefillId !== undefined ? { id: prefillId } : {}), ...(section !== undefined ? { section } : {}) }}
      {...(onTypeChosen !== undefined ? { onTypeChosen } : {})}
    />
  );
}
