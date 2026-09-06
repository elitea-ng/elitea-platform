/**
 * pages/credentials/useCredentialConnectionTest.ts — the "Test connection"
 * control's own state machine, split out of `./useCredentialFormController.ts`
 * (which was at 388 of its 400-line §3.5 budget before this landed).
 *
 * ## Why a saved credential needs a DIFFERENT call
 *
 * `POST /configurations/check_connection/{projectId}/{configType}` tests a
 * payload the CLIENT sends. That works exactly once — while the user is still
 * typing the secret. Every read path returns a stored credential with its
 * secret SEALED: `data.api_key` comes back as a `{{secret.NAME}}` reference,
 * never the key. So on the edit screen the Test button had nothing real to
 * send: it either posted the literal template string (asking the provider to
 * authenticate `{{secret.openai_key}}`, which reports a working credential as
 * broken) or it required the user to re-type a key the platform already has —
 * which teaches, by accident, that the platform will test a key it was never
 * given.
 *
 * `POST /configurations/check_stored_connection/{projectId}/{configId}` is the
 * answer, and its discriminating property is what this module exists to
 * preserve: THE REQUEST CARRIES NO BODY AND NO SECRET. The server reads the
 * row, redeems the reference through the project vault, and dials the
 * provider itself. Nothing secret leaves the browser, because the browser
 * does not have it. `EditCredential.test.tsx`'s recorded-request assertions
 * pin exactly that — an empty request body, on the wire.
 *
 * ## Which of the two runs
 *
 * The secret field's dirtiness decides, not the route:
 *
 *   - edit screen, secret field UNTOUCHED  → stored check (no body).
 *   - the user has typed a new secret       → unsaved check with that value,
 *     because the candidate the user wants tested is the one in the box, and
 *     it is not the one on the server yet.
 *   - create screen                          → unsaved check, always: there
 *     is no saved row to name.
 *
 * `noteFieldChanged` is how the form reports typing; the controller calls it
 * from its own `setField`, so one edit to any secret-shaped field flips this
 * hook to the payload form for the rest of the session.
 */
import { useCallback, useMemo, useRef, useState } from 'react';

import { CredentialConnectionChecks, SchemaField } from '@/features/credentials';
import { t } from '@/shared/i18n';
import type { ConfigSchemaNode } from '@/features/credentials';

/**
 * `unsupported` is its own outcome, not a failure. A credential type this
 * build has no checker for has not failed anything — reporting it in the
 * error colour tells the user to go fix a credential that is fine.
 */
type CredentialTestResult = 'idle' | 'success' | 'failure' | 'unsupported';

type TestOutcome =
  | { readonly status: 'success' }
  | { readonly status: 'failure'; readonly message: string }
  | { readonly status: 'unsupported'; readonly message: string };

export interface CredentialConnectionTestParams {
  readonly projectId: string;
  /** The saved row's id — `undefined` on the create screen, which is what makes the stored form unavailable there. */
  readonly configId: string | undefined;
  readonly configType: string | undefined;
  readonly data: Readonly<Record<string, unknown>>;
  /** Used only to work out which field keys are secret-shaped (`SchemaField.classify(...) === 'secret'`). */
  readonly schemaProperties: Readonly<Record<string, ConfigSchemaNode>>;
}

export interface CredentialConnectionTestResult {
  readonly testResult: CredentialTestResult;
  readonly testMessage: string;
  readonly isTesting: boolean;
  readonly testConnection: () => void;
  readonly noteFieldChanged: (fieldKey: string) => void;
}

/** `EliteaApiError.failure.body` for an `'http'`-kind failure — the parsed 400/404 body. Duck-typed locally, the same way `features/credentials/model/useCredentialValidation.ts` and `./useCredentialFormController.ts` already duck-type this exact shape. */
function httpFailureBody(error: unknown): Record<string, unknown> | undefined {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return undefined;
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null) return undefined;
  const record = failure as { kind?: unknown; body?: unknown };
  if (record.kind !== 'http') return undefined;
  const { body } = record;
  return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : undefined;
}

/** `message` first (the stored-check contract's own field), then `error` (the unsaved check's). Non-empty strings only, so an absent message falls through to the caller's own wording rather than rendering `""`. */
function readMessage(source: Readonly<Record<string, unknown>> | undefined): string | undefined {
  const value = source?.['message'] ?? source?.['error'];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function isUnsupported(source: Readonly<Record<string, unknown>> | undefined): boolean {
  return source?.['unsupported'] === true;
}

function unsupportedMessage(source: Readonly<Record<string, unknown>> | undefined): string {
  return readMessage(source) ?? t('credentials.form.testUnsupported', 'Checking connection is not supported yet for this configuration type.');
}

/**
 * The stored form. NO BODY IS BUILT HERE — `mutateAsync` takes ids only, and
 * the fetcher it reaches (`features/credentials/api/configurations.ts`'s
 * `checkStoredConfigurationConnection`) sends `{ method: 'POST' }` and
 * nothing else. There is deliberately no parameter on this function through
 * which form data could reach the wire.
 */
async function performStoredTest(
  storedCheck: ReturnType<typeof CredentialConnectionChecks.useStored>,
  projectId: string,
  configId: string,
): Promise<TestOutcome> {
  try {
    const result = await storedCheck.mutateAsync({ projectId, configId });
    if (isUnsupported(result)) return { status: 'unsupported', message: unsupportedMessage(result) };
    // The route answers 200 only on a proven round trip, so `success` is true
    // here; the explicit `=== false` branch is for a future body that reports
    // a soft failure at 200 rather than at 400.
    if (result.success === false) return { status: 'failure', message: readMessage(result) ?? t('credentials.form.testFailed', 'Connection test failed') };
    return { status: 'success' };
  } catch (error) {
    // A failed check is HTTP 400 `{success:false,message}` (and a row this
    // project does not own is a 404 of the same shape), so the interesting
    // case arrives as a throw, not as a resolved value. Dropping the body
    // here would replace the server's real reason with a generic string.
    const body = httpFailureBody(error);
    if (isUnsupported(body)) return { status: 'unsupported', message: unsupportedMessage(body) };
    return { status: 'failure', message: readMessage(body) ?? t('credentials.form.testFailed', 'Connection test failed') };
  }
}

/** The unsaved form — unchanged behaviour from this hook's previous home in `useCredentialFormController.ts`: a 2xx body carrying `error` is the failure signal. */
async function performUnsavedTest(
  unsavedCheck: ReturnType<typeof CredentialConnectionChecks.useUnsaved>,
  projectId: string,
  configType: string,
  data: Readonly<Record<string, unknown>>,
): Promise<TestOutcome> {
  try {
    const result = await unsavedCheck.mutateAsync({ projectId, configType, body: data });
    if (result.error) return { status: 'failure', message: result.error };
    return { status: 'success' };
  } catch (error) {
    const body = httpFailureBody(error);
    if (isUnsupported(body)) return { status: 'unsupported', message: unsupportedMessage(body) };
    return { status: 'failure', message: readMessage(body) ?? t('credentials.form.testFailed', 'Connection test failed') };
  }
}

/**
 * Picks the form and starts it, or returns `undefined` when neither applies
 * (a create screen whose type has not resolved yet). A free function, not an
 * inline branch, so the narrowing that proves `configId`/`configType` are
 * present is the same expression that passes them.
 */
function startConnectionTest(
  checks: {
    readonly stored: ReturnType<typeof CredentialConnectionChecks.useStored>;
    readonly unsaved: ReturnType<typeof CredentialConnectionChecks.useUnsaved>;
  },
  request: {
    readonly projectId: string;
    readonly configId: string | undefined;
    readonly configType: string | undefined;
    readonly data: Readonly<Record<string, unknown>>;
    readonly secretTouched: boolean;
  },
): Promise<TestOutcome> | undefined {
  // A saved row whose secret the user has NOT re-typed is the only case the
  // stored route applies to — and it is the common one, since the sealed
  // value the form loaded is not a secret the browser could send anyway.
  if (request.configId !== undefined && !request.secretTouched) {
    return performStoredTest(checks.stored, request.projectId, request.configId);
  }
  if (request.configType === undefined) return undefined;
  return performUnsavedTest(checks.unsaved, request.projectId, request.configType, request.data);
}

export function useCredentialConnectionTest(params: CredentialConnectionTestParams): CredentialConnectionTestResult {
  const { projectId, configId, configType, data, schemaProperties } = params;

  const [testResult, setTestResult] = useState<CredentialTestResult>('idle');
  const [testMessage, setTestMessage] = useState('');
  const [secretTouched, setSecretTouched] = useState(false);

  const secretFieldKeys = useMemo(
    () => new Set(Object.entries(schemaProperties).filter(([key, property]) => SchemaField.classify(key, property) === 'secret').map(([key]) => key)),
    [schemaProperties],
  );
  // Read through a ref so `noteFieldChanged` stays referentially stable across
  // schema loads — the controller wires it into its own `setField`, whose
  // stability the whole form's memoization depends on.
  const secretFieldKeysRef = useRef(secretFieldKeys);
  secretFieldKeysRef.current = secretFieldKeys;

  const noteFieldChanged = useCallback((fieldKey: string): void => {
    if (secretFieldKeysRef.current.has(fieldKey)) setSecretTouched(true);
  }, []);

  const unsavedCheck = CredentialConnectionChecks.useUnsaved();
  const storedCheck = CredentialConnectionChecks.useStored();

  // One memoized object instead of five separate `testConnection`
  // dependencies — the same collapse `useCredentialFormController.ts`'s
  // `formValues` makes, and for the same §3.5 hook-deps budget reason.
  const request = useMemo(
    () => ({ projectId, configId, configType, data, secretTouched }),
    [projectId, configId, configType, data, secretTouched],
  );

  const testConnection = useCallback(() => {
    setTestResult('idle');
    const run = startConnectionTest({ stored: storedCheck, unsaved: unsavedCheck }, request);
    if (run === undefined) return;
    void run.then((outcome) => {
      setTestResult(outcome.status);
      setTestMessage(outcome.status === 'success' ? '' : outcome.message);
    });
  }, [storedCheck, unsavedCheck, request]);

  return {
    testResult,
    testMessage,
    isTesting: unsavedCheck.isPending || storedCheck.isPending,
    testConnection,
    noteFieldChanged,
  };
}
