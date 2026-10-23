import { useEffect, useRef, useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { assessDiscovery } from '@/api/discovery';
import { devSignInEnabled, signInWithPat, type DevSignInResult } from '@/auth/devSignIn';
import { signInWithWorkspace, type NativeSignInError } from '@/auth/nativeAuth';
import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { SignInView, type NativeSignInState } from '@/features/entry/SignInView';
import { useActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { useSession } from '@/state/session';
import { leaveSignIn } from '@/state/workspace';
import { refreshWorkspaceInfo, useWorkspaceInfo } from '@/state/workspaceInfo';
import { useBrandTheme } from '@/theme/brandStore';
import { Button, Text, TextField } from '@/ui';

type SignInError = Extract<DevSignInResult, { ok: false }>['error'];

const ERROR_TEXT: Record<SignInError, string> = {
  empty: 'Paste a personal access token.',
  disabled: 'Developer sign-in is not available in this build.',
  not_connected: 'Choose a workspace first.',
  rejected: 'Token rejected.',
  unreachable: "Can't reach server.",
  no_project: 'This account has no active project in the workspace.',
  storage: 'The token could not be stored securely on this device.',
  unexpected: 'Sign-in failed. Try again.',
};

const NATIVE_ERROR_TEXT: Record<NativeSignInError, string | null> = {
  busy: null,
  cancelled: null,
  not_available: `This workspace's sign-in for ${identity.displayName} is not available right now.`,
  state_mismatch: 'The sign-in response did not match this attempt. Try again.',
  issuer_mismatch: 'The sign-in response came from a different server. Try again.',
  denied: 'Sign-in was declined.',
  server_error: 'The workspace could not complete the sign-in. Try again.',
  invalid_grant: 'The sign-in expired or was already used. Try again.',
  invalid_client: `This workspace no longer accepts ${identity.displayName}. Ask your administrator.`,
  upgrade_required: null, // the update screen takes over
  unavailable: 'The workspace is busy. Try again in a moment.',
  network: "Can't reach the workspace. Check your connection and try again.",
  storage: 'The sign-in could not be stored securely on this device.',
  workspace_changed: null,
};

/**
 * "Sign in": the workspace's native sign-in (ADR-0025 WP2: authorization
 * code + PKCE S256 in the system browser). Development builds (`__DEV__`) can
 * also paste a personal access token, behind "Other sign-in options".
 */
export default function SignInScreen() {
  const serverOrigin = useSession((s) => s.serverOrigin);
  const enabled = devSignInEnabled();
  const workspace = useActiveWorkspace();
  const info = useWorkspaceInfo();
  const native = useNativeSignIn();
  const brandLogo = useBrandLogo(serverOrigin);
  if (serverOrigin === null) return null;
  const host = displayHost(serverOrigin);
  const current = info !== null && info.origin === serverOrigin ? info : null;
  return (
    <SignInView
      host={host}
      origin={serverOrigin}
      name={current?.discovery.display_name || workspace?.name || host}
      logo={brandLogo}
      brand={workspace?.brand ?? null}
      native={native}
      developer={enabled ? <DevPatForm /> : null}
      onUseDifferentWorkspace={() => void leaveSignIn()}
    />
  );
}

/** The workspace's logo for the sign-in tile: its mark, else its full logo. */
function useBrandLogo(origin: string | null) {
  const brand = useBrandTheme();
  return brand !== null && brand.origin === origin ? (brand.logoMark ?? brand.logoFull) : null;
}

function useNativeSignIn(): NativeSignInState {
  const serverOrigin = useSession((s) => s.serverOrigin);
  const info = useWorkspaceInfo();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const current = info !== null && info.origin === serverOrigin ? info : null;
  const load = async () => {
    setLoading(true);
    await refreshWorkspaceInfo();
    if (mounted.current) setLoading(false);
  };

  const onSignIn = async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    const result = await signInWithWorkspace();
    if (!mounted.current) return;
    setBusy(false);
    // On success the root layout's guards route to the signed-in area.
    if (!result.ok) setError(NATIVE_ERROR_TEXT[result.error]);
  };

  if (current === null) return loading ? { kind: 'loading' } : { kind: 'unloaded', onRetry: () => void load() };
  const assessment = assessDiscovery(current.discovery);
  if (assessment.kind === 'native_auth_unavailable' || assessment.kind === 'contract_unsupported') {
    return {
      kind: 'unavailable',
      text:
        assessment.kind === 'native_auth_unavailable'
          ? `This workspace hasn't enabled ${identity.displayName} sign-in. Ask your administrator to register the app.`
          : `This workspace needs a different version of ${identity.displayName}.`,
    };
  }
  return { kind: 'ready', busy, error, onSignIn: () => void onSignIn() };
}

function DevPatForm() {
  const { colors } = useDesign();
  const [token, setToken] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mounted = useRef(true);

  // The token never outlives this screen.
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const onSubmit = async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    const submitted = token;
    setToken('');
    const result = await signInWithPat(submitted);
    if (!mounted.current) return;
    setBusy(false);
    if (!result.ok) setError(ERROR_TEXT[result.error]);
    // On success the root layout's guards route to the signed-in area.
  };

  return (
    <View style={styles.form}>
      <View style={[styles.banner, { backgroundColor: colors.dangerSoft }]}>
        <Text variant="subheadline" tone="danger" emphasized>
          Developer sign-in
        </Text>
        <Text variant="footnote" tone="secondary">
          Paste a personal access token from your workspace profile. It is stored only on this
          device and grants full access to your account.
        </Text>
      </View>
      <TextField
        label="Personal access token"
        value={token}
        onChangeText={(text) => {
          setToken(text);
          setError(null);
        }}
        onBlur={() => {
          if (!busy) setToken('');
        }}
        onSubmitEditing={() => void onSubmit()}
        editable={!busy}
        secureTextEntry
        autoCapitalize="none"
        autoCorrect={false}
        autoComplete="off"
        textContentType="none"
        importantForAutofill="no"
        spellCheck={false}
        returnKeyType="go"
        error={error}
      />
      <Button title="Sign in with token" variant="tonal" loading={busy} onPress={() => void onSubmit()} />
    </View>
  );
}

const styles = StyleSheet.create({
  form: { gap: 12 },
  banner: { borderRadius: 16, borderCurve: 'continuous', padding: 14, gap: 4 },
});
