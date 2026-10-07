import { useEffect, useRef, useState } from 'react';
import { KeyboardAvoidingView, Platform, ScrollView, StyleSheet, TextInput, View } from 'react-native';
import { SafeAreaView } from 'react-native-safe-area-context';

import { assessDiscovery } from '@/api/discovery';
import { devSignInEnabled, signInWithPat, type DevSignInResult } from '@/auth/devSignIn';
import { signInWithWorkspace, type NativeSignInError } from '@/auth/nativeAuth';
import { identity } from '@/config/identity';
import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { HostText } from '@/features/connect/HostText';
import { useSession } from '@/state/session';
import { leaveSignIn } from '@/state/workspace';
import { refreshWorkspaceInfo, useWorkspaceInfo } from '@/state/workspaceInfo';
import { BrandLogo } from '@/theme/BrandLogo';
import { useBrandTheme } from '@/theme/brandStore';
import { Button, ramp, Text } from '@/ui';

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
 * also paste a personal access token.
 */
export default function SignInScreen() {
  const palette = usePalette();
  const serverOrigin = useSession((s) => s.serverOrigin);
  const enabled = devSignInEnabled();
  const brand = useBrandTheme();

  return (
    <SafeAreaView style={[styles.flex, { backgroundColor: palette.background }]}>
      <KeyboardAvoidingView
        style={styles.flex}
        behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        <ScrollView contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
          <BrandLogo image={brand?.logoMark ?? brand?.logoFull ?? null} size={56} />
          <Text variant="title1" heading style={styles.title}>
            Sign in
          </Text>
          <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
            <Text variant="footnote" tone="secondary" emphasized>
              Workspace
            </Text>
            {serverOrigin ? <HostText host={displayHost(serverOrigin)} role="title2" color={palette.text} /> : null}
          </View>

          <NativeSignIn />

          {enabled ? <DevPatForm /> : null}

          <Button title="Use a different workspace" variant="plain" onPress={() => void leaveSignIn()} />
        </ScrollView>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}

function NativeSignIn() {
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

  if (current === null) {
    return (
      <View style={styles.form}>
        <Text tone="secondary">
          {loading ? 'Checking the workspace…' : "The workspace's sign-in details are not loaded."}
        </Text>
        <Button title="Try again" variant="tonal" loading={loading} onPress={() => void load()} />
      </View>
    );
  }

  const assessment = assessDiscovery(current.discovery);
  if (assessment.kind === 'native_auth_unavailable' || assessment.kind === 'contract_unsupported') {
    return (
      <Text accessibilityRole="alert" tone="secondary">
        {assessment.kind === 'native_auth_unavailable'
          ? `This workspace hasn't enabled ${identity.displayName} sign-in. Ask your administrator to register the app.`
          : `This workspace needs a different version of ${identity.displayName}.`}
      </Text>
    );
  }

  const name = current.discovery.display_name || displayHost(current.origin);
  return (
    <View style={styles.form}>
      <Button testID="native-sign-in" title={`Sign in with ${name}`} loading={busy} onPress={() => void onSignIn()} />
      <Text variant="footnote" tone="secondary">
        Opens your organization&apos;s sign-in page in the system browser. {identity.displayName} never
        sees your password.
      </Text>
      {error ? (
        <Text accessibilityRole="alert" variant="subheadline" tone="danger">
          {error}
        </Text>
      ) : null}
    </View>
  );
}

function DevPatForm() {
  const palette = usePalette();
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
      <View style={[styles.banner, { borderColor: palette.danger }]}>
        <Text variant="subheadline" tone="danger" emphasized>
          Developer sign-in
        </Text>
        <Text variant="footnote" tone="secondary">
          Paste a personal access token from your workspace profile. It is stored only on this
          device and grants full access to your account.
        </Text>
      </View>
      <Text variant="footnote" tone="secondary" emphasized>
        Personal access token
      </Text>
      <TextInput
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
        accessibilityLabel="Personal access token"
        style={[
          styles.input,
          {
            color: palette.text,
            backgroundColor: palette.field,
            borderColor: error ? palette.danger : palette.inputBorder,
          },
        ]}
      />
      {error ? (
        <Text accessibilityRole="alert" variant="subheadline" tone="danger">
          {error}
        </Text>
      ) : null}
      <Button title="Sign in" loading={busy} onPress={() => void onSubmit()} />
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: {
    padding: Spacing.four,
    gap: Spacing.three,
    width: '100%',
    maxWidth: MaxContentWidth,
    alignSelf: 'center',
  },
  title: { marginTop: Spacing.five },
  card: { borderRadius: 16, borderCurve: 'continuous', padding: Spacing.three, gap: Spacing.one },
  form: { gap: Spacing.three },
  banner: { borderWidth: 1, borderRadius: 16, borderCurve: 'continuous', padding: Spacing.three, gap: Spacing.one },
  input: {
    ...ramp.body,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.three - 4,
  },
});
