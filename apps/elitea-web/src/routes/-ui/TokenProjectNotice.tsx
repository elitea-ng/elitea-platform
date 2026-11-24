/**
 * What the create-personal-token page SAYS about the project binding, plus
 * the copy for the two failures that binding can cause
 * (`spec-llm-project-scope` §4, ADR-0018).
 *
 * This file holds no control. The product decision is that a new token binds
 * to the project the sidebar selects, so the page offers no second choice.
 * The page must still disclose the binding, because the user cannot get these
 * two facts back afterwards: the bound project pays for the calls of the
 * token, and no API changes the binding later.
 *
 * Lives in `routes/-ui/` — the `-`-prefixed helper convention TanStack's
 * generator ignores when it builds the route tree — because it is used by
 * exactly one route and pulling it out is what keeps that route inside the
 * §3.5 file-length and complexity budgets.
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  tokenMintRefusal,
  tokenProjectErrorCode,
  tokenScopeRefusal,
  type TokenMintRefusal,
  type TokenScopeRefusal,
} from '@/entities/token/model/selectors';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

/* ── create-failure copy (§4 error contract) ──────────────────────────── */

/**
 * `POST /auth/token/` answers the two project failures with the NESTED
 * envelope `{"error":{message,type,code}}`, and every other failure with the
 * FLAT `{"error":"…"}`. `tokenProjectErrorCode` reads the nested shape and
 * returns `null` for the flat one, so both stay usable: a project failure
 * names the project problem, anything else falls back to the generic line.
 *
 * Both project messages send the user to the SIDEBAR project selector. That
 * selector is the only project control the flow has.
 */
export function createTokenFailureMessage(error: unknown): string {
  /*
   * "Try again" IS WRONG ADVICE FOR A 503 HERE.
   *
   * elitea-main answers every /api/v2/auth/token route with 503
   * `{"error":"token service is not configured"}` while APPLICATION_SECRET_KEY
   * is empty — the key that signs personal tokens. Retrying can never succeed;
   * only an operator can fix it. A live deployment sat in exactly that state
   * and told the user to try again.
   */
  if (error instanceof EliteaApiError) {
    const { failure } = error;
    if ((failure.kind === 'http' || failure.kind === 'auth') && failure.status === 503) {
      return t(
        'entities.token.form.serviceUnavailable',
        'Personal tokens are turned off on this deployment. Ask your administrator to configure the token service.',
      );
    }
  }
  const refusal = tokenMintRefusal(error);
  if (refusal !== null) return mintRefusalMessage(refusal);
  const scopeRefusal = tokenScopeRefusal(error);
  if (scopeRefusal !== null) return scopeRefusalMessage(scopeRefusal);
  const code = tokenProjectErrorCode(error);
  if (code === 'project_forbidden') {
    return t(
      'entities.token.form.projectForbidden',
      'You are not a member of this project. Select a different project in the sidebar, then create the token again.',
    );
  }
  if (code === 'invalid_project_id') {
    return t(
      'entities.token.form.invalidProjectId',
      'This project is not a valid choice. Select a different project in the sidebar, then create the token again.',
    );
  }
  return t('entities.token.form.createFailed', 'The system did not create the token. Try again.');
}

/*
 * Issue 2559: the server refuses a mint by the CALLING credential. The
 * desktop app signs in with a native device session, so an administrator
 * meets `token_mint_native_admin` there; "try again" would be wrong advice,
 * the browser is the way.
 */
function mintRefusalMessage({ code, projectId }: TokenMintRefusal): string {
  switch (code) {
    case 'token_mint_native_admin':
      return t(
        'entities.token.form.mintNativeAdmin',
        'Your account has administrator permissions, so this app cannot create a personal token for it. Create it in the web app in a browser.',
      );
    case 'token_mint_wider_than_caller':
      /*
       * Name the project: "select that project" with no name gives the user
       * nothing to select. The browser is the exit if it is not selectable.
       */
      return projectId === null
        ? t(
            'entities.token.form.mintWiderThanCaller',
            'Your sign-in is limited to one project, so the token must be bound to it. Create the token in the web app in a browser.',
          )
        : t(
            'entities.token.form.mintWiderThanCallerProject',
            'Your sign-in is limited to the project with ID {{project}}. Select that project in the sidebar and create the token again, or create it in the web app in a browser.',
            { project: projectId },
          );
    case 'token_mint_caller_unresolved':
      return t(
        'entities.token.form.mintCallerUnresolved',
        'The system could not check your sign-in, so it did not create the token. Create it in the web app in a browser.',
      );
    case 'token_mint_browser_session_required':
      return t(
        'entities.token.form.mintBrowserRequired',
        'A personal token can be created only after you sign in with a browser, not with another token.',
      );
  }
}

/*
 * ADR-0059 R1.3a: the administration scopes were refused. The two scope
 * checks name the scopes; the credential checks say where to go instead.
 */
function scopeRefusalMessage({ code, scopes }: TokenScopeRefusal): string {
  if (scopes.length > 0) {
    return t(
      'entities.token.form.scopeRefused',
      'This token cannot have these administration permissions: {{scopes}}. Remove them and create the token again.',
      { scopes: scopes.join(', ') },
    );
  }
  if (code === 'token_scopes_browser_session_required' || code === 'token_scopes_sealed_session') {
    return t(
      'entities.token.form.scopeBrowserRequired',
      'Administration permissions can be given to a token only after you sign in with your own account in the web app.',
    );
  }
  return t(
    'entities.token.form.scopeNotSettable',
    'This sign-in cannot give a token administration permissions. Remove them, or sign in again and retry.',
  );
}

/** A recent sign-in is needed, but this session has no browser sign-in to renew it. */
export function recentSignInUnavailableMessage(): string {
  return t(
    'entities.token.form.recentSignInUnavailable',
    'Administration permissions need a recent sign-in. Sign out, sign in again, and create the token within a few minutes.',
  );
}

/* ── the binding disclosure ────────────────────────────────────────────── */

/** The form's own field width, matching the name and expiration rows. */
const FIELD_SX: SxProps<Theme> = { width: '17.875rem' };
const HELPER_SX: SxProps<Theme> = {
  marginTop: '0.25rem',
  fontSize: ({ typography }) => typography.bodySmall.fontSize,
};

export interface TokenProjectNoticeProps {
  /**
   * The name of the project the token binds to, or `null` for an unbound
   * token. The caller derives this from the id it actually sends, never from
   * the store alone: a store value the request drops must not read here as a
   * binding.
   */
  readonly projectName: string | null;
}

export function TokenProjectNotice({ projectName }: TokenProjectNoticeProps) {
  return (
    <Box sx={FIELD_SX}>
      <Typography
        variant="bodySmall"
        color="text.secondary"
        sx={HELPER_SX}
      >
        {projectName === null
          ? t('entities.token.form.projectNoneNote', 'This token gets no project, and you cannot change this later.')
          : t(
              'entities.token.form.projectNote',
              'The project {{project}} pays for this token, and you cannot change this later.',
              { project: projectName },
            )}
      </Typography>
    </Box>
  );
}

/* ── the failure line ──────────────────────────────────────────────────── */

export interface TokenCreateErrorProps {
  readonly message: string | null;
}

export function TokenCreateError({ message }: TokenCreateErrorProps) {
  if (message === null) return null;
  return (
    <Box
      role="alert"
      sx={FIELD_SX}
    >
      <Typography
        variant="bodySmall"
        color="error"
      >
        {message}
      </Typography>
    </Box>
  );
}
