/**
 * The web Microsoft sign-in (ADR-0057 decision 6), as one hook.
 *
 * `login()` runs the whole flow from a click:
 *
 *  1. open the popup at once (a popup opened after an `await` is blocked), blank;
 *  2. save the credential first (`ensureSaved`): the server starts a sign-in for a saved
 *     credential only, and it reads the credential's own client, authority and scopes;
 *  3. ask Main to start (`microsoft_login:start`) and wait for the callback page's
 *     handoff, on postMessage and on the BroadcastChannel, matched on `state`;
 *  4. point the popup at Microsoft;
 *  5. redeem the handoff with the session (`microsoft_login:redeem`).
 *
 * The result goes to `onSignedIn`, which stores the access token where runs read it. A
 * popup that Entra detached from this window cannot be seen closing, so a sign-in the
 * person abandons ends by `cancel()` or by its own expiry (10 minutes), never by guessing.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { redeemSignIn, refusalCode, startSignIn } from '../api/microsoftLoginApi';
import type { MicrosoftLoginRedeemed, MicrosoftLoginStart } from '../api/microsoftLoginApi';
import { signInTokenKey } from '../lib/credentialKey';
import { waitForHandoff } from '../lib/handoff';
import { forgetSession, readSession, rememberSession, sessionOf } from '../lib/session';
import type { LoginSession } from '../lib/session';

type LoginPhase = 'idle' | 'saving' | 'starting' | 'waiting' | 'redeeming' | 'signedIn' | 'failed';

/** What a finished sign-in hands to the caller. */
export interface SignedIn {
  readonly credentialUuid: string;
  readonly redeemed: MicrosoftLoginRedeemed;
  /** The key the access token belongs under (`{uuid}:{authority}`), or '' when no authority could be named. */
  readonly tokenKey: string;
}

export interface UseMicrosoftLoginParams {
  readonly projectId: number | undefined;
  /** The saved credential's uuid, when the credential already exists. */
  readonly credentialUuid: string | undefined;
  /** Saves the credential and returns its uuid; `undefined` when it could not be saved. */
  readonly ensureSaved: () => Promise<string | undefined>;
  /** The form's current values: the sign-in's token key follows the mode (Custom or Default app) they select. */
  readonly credentialData: Readonly<Record<string, unknown>>;
  readonly onSignedIn: (result: SignedIn) => void;
}

export interface UseMicrosoftLoginResult {
  readonly phase: LoginPhase;
  /** The code of the last failure (see `failureMessage`), while `phase` is `failed`. */
  readonly failure: string | undefined;
  readonly session: LoginSession | undefined;
  readonly busy: boolean;
  readonly login: () => void;
  readonly cancel: () => void;
  /** Forget the sign-in's display facts (the caller drops the token). */
  readonly clear: () => void;
}

const POPUP_NAME = 'elitea-microsoft-login';
const POPUP_FEATURES = 'popup=yes,width=520,height=720';

class SignInFailure extends Error {
  readonly code: string;
  constructor(code: string) {
    super(code);
    this.code = code;
  }
}

function failureCodeOf(caught: unknown): string {
  if (caught instanceof SignInFailure) return caught.code;
  if (caught instanceof Error && (caught.message === 'expired' || caught.message === 'cancelled')) return caught.message;
  return refusalCode(caught) ?? 'generic';
}

/** Microsoft is reached over https only; a start answer that says otherwise is not followed. */
function safeAuthorizationUrl(raw: string): string {
  try {
    if (new URL(raw).protocol === 'https:') return raw;
  } catch {
    // Falls through to the refusal below.
  }
  throw new SignInFailure('generic');
}

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw new SignInFailure('cancelled');
}

interface FlowParams extends Pick<UseMicrosoftLoginParams, 'ensureSaved'> {
  readonly dataRef: { readonly current: Readonly<Record<string, unknown>> };
  readonly projectId: number;
  readonly popup: Window;
  readonly signal: AbortSignal;
  readonly setPhase: (phase: LoginPhase) => void;
}

/** Steps 2 to 5 of the flow. Throws a `SignInFailure`, a refusal, or an `expired`/`cancelled` error. */
async function runFlow({ projectId, ensureSaved, dataRef, popup, signal, setPhase }: FlowParams): Promise<SignedIn> {
  setPhase('saving');
  const credentialUuid = await ensureSaved();
  throwIfAborted(signal);
  if (credentialUuid === undefined) throw new SignInFailure('save_failed');

  setPhase('starting');
  const start: MicrosoftLoginStart = await startSignIn({ projectId, credentialId: credentialUuid });
  throwIfAborted(signal);
  const authorizationUrl = safeAuthorizationUrl(start.authorization_url);

  // Listen before the popup leaves for Microsoft, so no message can be missed.
  const wait = waitForHandoff({
    state: start.state,
    channelName: start.channel_name,
    timeoutMs: Math.max(1_000, Date.parse(start.expires_at) - Date.now()),
    popup,
    signal,
  });
  setPhase('waiting');
  popup.location.href = authorizationUrl;
  const outcome = await wait.outcome;
  if (outcome.kind === 'error') throw new SignInFailure(outcome.error);

  setPhase('redeeming');
  const redeemed = await redeemSignIn({ projectId, handoff: outcome.handoff, state: start.state });
  const tokenKey = signInTokenKey({ credentialUuid, data: dataRef.current, authorizationUrl }) ?? '';
  return { credentialUuid, redeemed, tokenKey };
}

export function useMicrosoftLogin(params: UseMicrosoftLoginParams): UseMicrosoftLoginResult {
  const { projectId, credentialUuid, ensureSaved, credentialData, onSignedIn } = params;
  const dataRef = useRef(credentialData);
  dataRef.current = credentialData;
  const [phase, setPhase] = useState<LoginPhase>('idle');
  const [failure, setFailure] = useState<string | undefined>(undefined);
  const [session, setSession] = useState<LoginSession | undefined>(() => readSession(credentialUuid));
  const abortRef = useRef<AbortController | null>(null);
  const popupRef = useRef<Window | null>(null);
  // The credential a sign-in finished for: on the create screen the form learns its uuid only after the save.
  const signedUuidRef = useRef<string | undefined>(undefined);
  const busy = phase === 'saving' || phase === 'starting' || phase === 'waiting' || phase === 'redeeming';

  // The credential's remembered sign-in follows the credential the form shows.
  useEffect(() => {
    setSession(readSession(credentialUuid));
  }, [credentialUuid]);

  // Leaving the form ends a sign-in in flight.
  useEffect(
    () => () => {
      abortRef.current?.abort();
    },
    [],
  );

  const closePopup = useCallback(() => {
    try {
      popupRef.current?.close();
    } catch {
      // A popup that already went away cannot be closed again.
    }
    popupRef.current = null;
  }, []);

  const finish = useCallback(
    (result: SignedIn) => {
      const next = sessionOf(result.redeemed, result.tokenKey);
      signedUuidRef.current = result.credentialUuid;
      rememberSession(result.credentialUuid, next);
      setSession(next);
      setFailure(undefined);
      setPhase('signedIn');
      onSignedIn(result);
    },
    [onSignedIn],
  );

  const login = useCallback(() => {
    // `abortRef` is set in this very click, so a second click before the next render is refused too.
    if (busy || abortRef.current !== null || projectId === undefined) return;
    const popup = window.open('about:blank', POPUP_NAME, POPUP_FEATURES);
    if (popup === null) {
      setFailure('popup_blocked');
      setPhase('failed');
      return;
    }
    popupRef.current = popup;
    const controller = new AbortController();
    abortRef.current = controller;
    setFailure(undefined);
    runFlow({ projectId, ensureSaved, dataRef, popup, signal: controller.signal, setPhase })
      .then(finish)
      .catch((caught: unknown) => {
        closePopup();
        if (abortRef.current !== controller) return;
        const code = failureCodeOf(caught);
        // Cancelling is the person's own act, not a failure to explain.
        setFailure(code === 'cancelled' ? undefined : code);
        setPhase(code === 'cancelled' ? 'idle' : 'failed');
      })
      .finally(() => {
        if (abortRef.current === controller) abortRef.current = null;
      });
  }, [busy, projectId, ensureSaved, finish, closePopup]);

  const cancel = useCallback(() => {
    abortRef.current?.abort();
    closePopup();
  }, [closePopup]);

  const clear = useCallback(() => {
    const uuid = credentialUuid ?? signedUuidRef.current;
    if (uuid !== undefined) forgetSession(uuid);
    setSession(undefined);
    setFailure(undefined);
    setPhase('idle');
  }, [credentialUuid]);

  return { phase, failure, session, busy, login, cancel, clear };
}
