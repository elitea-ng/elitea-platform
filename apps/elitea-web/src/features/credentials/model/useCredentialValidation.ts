/**
 * model/useCredentialValidation.ts — on-demand credential connection-test
 * status, cached per credential id (unit A7). Ported from
 * `apps/elitea-ui/src/[fsd]/features/credentials/lib/hooks/useCredentialValidation.hooks.js`.
 *
 * Placed in `model/` (not `api/`): this is client-only UI state (a status
 * cache keyed by credential id) layered ON TOP of the two mutation hooks in
 * `../api/useConfigurations.ts` — R-S1 is satisfied because nothing here is
 * data a query already holds (the mutation results are transient outcomes,
 * not a cached resource). A plain `useState`-backed hook, not a zustand
 * store: the baseline hook is called once per `CredentialsSelect` instance
 * and its state was never shared across components, so a module-scope
 * store would be a behaviour change, not a straight port.
 */
import { useCallback, useRef, useState } from 'react';

import {
  useBatchTestConfigurationConnection,
  useBatchCheckStoredConfigurationConnections,
  useCheckStoredConfigurationConnection,
  useTestConfigurationConnection,
} from '../api/useConfigurations';

import { applyStoredRow, getHttpErrorMessage, getHttpFailureBody, getHttpStatus } from './credentialValidation.helpers';
import { isCredentialRefusalReason } from './credentialValidationStatus';
import type { CredentialValidationStatus } from './credentialValidationStatus';

export type { CredentialValidationStatus } from './credentialValidationStatus';

interface ValidateCredentialParams {
  readonly projectId: string | number;
  readonly credentialId: string;
  readonly credentialType: string;
  readonly data: Readonly<Record<string, unknown>>;
}

interface BatchValidateCredentialItem {
  readonly projectId: string | number;
  readonly credentialId: string;
  readonly credentialType: string;
  readonly data: Readonly<Record<string, unknown>>;
}

/**
 * One SAVED row to check.
 *
 * `configId` is the row's own id, and it is the whole difference between this
 * and {@link ValidateCredentialParams}: the stored routes carry no body, so the
 * server reads the row and redeems its sealed secret itself. `credentialId`
 * stays the key the statuses are stored under (the picker reads them back by
 * `eliteaTitle`, not by row id — see `pages/toolkits/lib/credentialPicker.tsx`).
 */
interface StoredValidateCredentialItem {
  readonly projectId: string | number;
  readonly credentialId: string;
  readonly configId: string;
}

export interface UseCredentialValidationResult {
  validateCredential: (params: ValidateCredentialParams) => Promise<void>;
  batchValidateCredentials: (items: readonly BatchValidateCredentialItem[]) => Promise<void>;
  /**
   * The SAVED-row form of {@link UseCredentialValidationResult.validateCredential}.
   *
   * Every read path seals a stored secret as a `{{secret.NAME}}` reference
   * (`internal/api/v2/configurations/secret_sealing.go`), so re-checking a saved
   * row through the unsaved route asks the provider to authenticate a literal
   * template string — which fails for a credential that works, and, worse,
   * "succeeds" at staying red after the user has fixed it. Any caller holding a
   * row id must use this one.
   */
  validateStoredCredential: (item: StoredValidateCredentialItem) => Promise<void>;
  /** The batch form of {@link UseCredentialValidationResult.validateStoredCredential}, one request per project. */
  batchValidateStoredCredentials: (items: readonly StoredValidateCredentialItem[]) => Promise<void>;
  getCredentialStatus: (credentialId: string | undefined) => CredentialValidationStatus;
  getCredentialMessage: (credentialId: string | undefined) => string;
  /**
   * The refusal REASON the toolkit probe answered with, and ONLY when that
   * reason is a verdict about the credential itself (`auth_failed` /
   * `unreachable`). `undefined` for every other outcome — a healthy
   * credential, a type with no probe, and, crucially, a refusal this
   * deployment could not really make ("Connection checking is not available
   * right now."), which carries no reason at all.
   *
   * Gate an action on THIS, never on `getCredentialStatus() === 'invalid'`:
   * the status cannot tell "the provider rejected this key" from "nobody
   * asked", and a gate built on it refuses the user's work because of the
   * deployment's own missing dependency. See `credentialValidationStatus.ts`.
   */
  getCredentialRefusalReason: (credentialId: string | undefined) => string | undefined;
  resetStatus: (credentialId: string) => void;
  resetStatuses: () => void;
}

/** HTTP statuses the baseline treats as "this credential type has no test-connection support", not a failure. */
const UNSUPPORTED_STATUSES = new Set([404, 405, 501]);

export function useCredentialValidation(): UseCredentialValidationResult {
  const [statuses, setStatuses] = useState<Record<string, CredentialValidationStatus>>({});
  const [messages, setMessages] = useState<Record<string, string>>({});
  const [reasons, setReasons] = useState<Record<string, string>>({});
  const statusesRef = useRef(statuses);
  statusesRef.current = statuses;

  const testConnection = useTestConfigurationConnection();
  const batchTestConnection = useBatchTestConfigurationConnection();
  const storedCheck = useCheckStoredConfigurationConnection();
  const batchStoredCheck = useBatchCheckStoredConfigurationConnections();

  const validateCredential = useCallback(
    async ({ projectId, credentialId, credentialType, data }: ValidateCredentialParams): Promise<void> => {
      const currentStatus = statusesRef.current[credentialId];
      if (currentStatus === 'checking' || currentStatus === 'valid' || currentStatus === 'invalid' || currentStatus === 'unsupported') {
        return;
      }

      statusesRef.current = { ...statusesRef.current, [credentialId]: 'checking' };
      setStatuses((prev) => ({ ...prev, [credentialId]: 'checking' }));

      try {
        const result = await testConnection.mutateAsync({ projectId, configType: credentialType, body: data });
        const isValid = result.error === undefined;
        setStatuses((prev) => ({ ...prev, [credentialId]: isValid ? 'valid' : 'invalid' }));
        if (!isValid && result.error) {
          setMessages((prev) => ({ ...prev, [credentialId]: result.error ?? '' }));
        }
      } catch (error) {
        const status = getHttpStatus(error);
        if (status !== undefined && UNSUPPORTED_STATUSES.has(status)) {
          setStatuses((prev) => ({ ...prev, [credentialId]: 'unsupported' }));
          return;
        }
        setStatuses((prev) => ({ ...prev, [credentialId]: 'invalid' }));
        const message = getHttpErrorMessage(error);
        if (message !== undefined) {
          setMessages((prev) => ({ ...prev, [credentialId]: message }));
        }
      }
    },
    [testConnection],
  );

  const validateProjectBatch = useCallback(
    async (projectId: string | number, credentials: readonly BatchValidateCredentialItem[]): Promise<void> => {
      const items = credentials.map((c) => ({ id: c.credentialId, type: c.credentialType, data: c.data }));
      try {
        const rows = await batchTestConnection.mutateAsync({ projectId, items });
        const statusUpdates: Record<string, CredentialValidationStatus> = {};
        const messageUpdates: Record<string, string> = {};
        for (const row of rows) {
          if (row.unsupported) {
            statusUpdates[row.id] = 'unsupported';
          } else {
            statusUpdates[row.id] = row.success ? 'valid' : 'invalid';
            if (!row.success && row.message) messageUpdates[row.id] = row.message;
          }
        }
        setStatuses((prev) => ({ ...prev, ...statusUpdates }));
        if (Object.keys(messageUpdates).length > 0) setMessages((prev) => ({ ...prev, ...messageUpdates }));
      } catch {
        const statusUpdates: Record<string, CredentialValidationStatus> = {};
        for (const c of credentials) statusUpdates[c.credentialId] = 'invalid';
        setStatuses((prev) => ({ ...prev, ...statusUpdates }));
      }
    },
    [batchTestConnection],
  );

  const batchValidateCredentials = useCallback(
    async (items: readonly BatchValidateCredentialItem[]): Promise<void> => {
      const toValidate = items.filter((item) => {
        const currentStatus = statusesRef.current[item.credentialId];
        return currentStatus === undefined || currentStatus === 'idle';
      });
      if (toValidate.length === 0) return;

      const checkingUpdates: Record<string, CredentialValidationStatus> = {};
      const byProject = new Map<string, BatchValidateCredentialItem[]>();
      for (const item of toValidate) {
        checkingUpdates[item.credentialId] = 'checking';
        statusesRef.current = { ...statusesRef.current, [item.credentialId]: 'checking' };
        const key = String(item.projectId);
        const bucket = byProject.get(key) ?? [];
        bucket.push(item);
        byProject.set(key, bucket);
      }
      setStatuses((prev) => ({ ...prev, ...checkingUpdates }));

      await Promise.all([...byProject.entries()].map(([projectId, credentials]) => validateProjectBatch(projectId, credentials)));
    },
    [validateProjectBatch],
  );

  /**
   * The stored single check.
   *
   * A refusal is HTTP 400 (`{success:false,message,reason?}`), so it arrives as
   * a thrown `EliteaApiError` and the message is read off `failure.body` — the
   * shape `useCheckStoredConfigurationConnection`'s own doc comment states.
   * `reason: 'unsupported_type'` is NOT a bad credential: it is a type this
   * build carries no probe for, and it must not light the attention indicator.
   */
  const validateStoredCredential = useCallback(
    async ({ projectId, credentialId, configId }: StoredValidateCredentialItem): Promise<void> => {
      statusesRef.current = { ...statusesRef.current, [credentialId]: 'checking' };
      setStatuses((prev) => ({ ...prev, [credentialId]: 'checking' }));

      try {
        const result = await storedCheck.mutateAsync({ projectId, configId });
        applyStoredRow(credentialId, { success: result.success === true, ...result }, setStatuses, setMessages, setReasons);
      } catch (error) {
        const status = getHttpStatus(error);
        if (status !== undefined && UNSUPPORTED_STATUSES.has(status)) {
          setStatuses((prev) => ({ ...prev, [credentialId]: 'unsupported' }));
          return;
        }
        const body = getHttpFailureBody(error);
        const reason = typeof body === 'object' && body !== null ? (body as { reason?: unknown }).reason : undefined;
        applyStoredRow(
          credentialId,
          { success: false, reason: typeof reason === 'string' ? reason : undefined, message: getHttpErrorMessage(error) },
          setStatuses,
          setMessages,
          setReasons,
        );
      }
    },
    [storedCheck],
  );

  const validateStoredProjectBatch = useCallback(
    async (projectId: string | number, items: readonly StoredValidateCredentialItem[]): Promise<void> => {
      const byConfigId = new Map(items.map((item) => [item.configId, item.credentialId]));
      try {
        const rows = await batchStoredCheck.mutateAsync({ projectId, configurationIds: items.map((item) => item.configId) });
        for (const row of rows) {
          const credentialId = byConfigId.get(String(row.id));
          if (credentialId === undefined) continue;
          applyStoredRow(credentialId, row, setStatuses, setMessages, setReasons);
        }
      } catch {
        // The request failed, not the credentials. Marking every row invalid
        // here would paint a healthy project red — the same reason the server
        // answers this route 200 whatever happens to an individual item.
        const updates: Record<string, CredentialValidationStatus> = {};
        for (const item of items) updates[item.credentialId] = 'idle';
        setStatuses((prev) => ({ ...prev, ...updates }));
      }
    },
    [batchStoredCheck],
  );

  const batchValidateStoredCredentials = useCallback(
    async (items: readonly StoredValidateCredentialItem[]): Promise<void> => {
      const toValidate = items.filter((item) => {
        const currentStatus = statusesRef.current[item.credentialId];
        return currentStatus === undefined || currentStatus === 'idle';
      });
      if (toValidate.length === 0) return;

      const checkingUpdates: Record<string, CredentialValidationStatus> = {};
      const byProject = new Map<string, StoredValidateCredentialItem[]>();
      for (const item of toValidate) {
        checkingUpdates[item.credentialId] = 'checking';
        statusesRef.current = { ...statusesRef.current, [item.credentialId]: 'checking' };
        const key = String(item.projectId);
        const bucket = byProject.get(key) ?? [];
        bucket.push(item);
        byProject.set(key, bucket);
      }
      setStatuses((prev) => ({ ...prev, ...checkingUpdates }));

      await Promise.all([...byProject.entries()].map(([projectId, rows]) => validateStoredProjectBatch(projectId, rows)));
    },
    [validateStoredProjectBatch],
  );

  const getCredentialStatus = useCallback(
    (credentialId: string | undefined): CredentialValidationStatus => (credentialId === undefined ? 'idle' : (statuses[credentialId] ?? 'idle')),
    [statuses],
  );

  const getCredentialMessage = useCallback(
    (credentialId: string | undefined): string => (credentialId === undefined ? '' : (messages[credentialId] ?? '')),
    [messages],
  );

  const getCredentialRefusalReason = useCallback(
    (credentialId: string | undefined): string | undefined => {
      const reason = credentialId === undefined ? undefined : reasons[credentialId];
      return isCredentialRefusalReason(reason) ? reason : undefined;
    },
    [reasons],
  );

  const resetStatus = useCallback((credentialId: string): void => {
    setStatuses((prev) => {
      const next = { ...prev };
      delete next[credentialId];
      statusesRef.current = next;
      return next;
    });
    setMessages((prev) => {
      const next = { ...prev };
      delete next[credentialId];
      return next;
    });
    setReasons((prev) => {
      const next = { ...prev };
      delete next[credentialId];
      return next;
    });
  }, []);

  const resetStatuses = useCallback((): void => {
    setStatuses({});
    setMessages({});
    setReasons({});
  }, []);

  return {
    validateCredential,
    batchValidateCredentials,
    validateStoredCredential,
    batchValidateStoredCredentials,
    getCredentialStatus,
    getCredentialMessage,
    getCredentialRefusalReason,
    resetStatus,
    resetStatuses,
  };
}
