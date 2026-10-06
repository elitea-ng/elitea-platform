import { useEffect, useRef, useState } from 'react';
import {
  ActivityIndicator,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { SafeAreaView } from 'react-native-safe-area-context';

import { assessDiscovery } from '@/api/discovery';
import { devSignInEnabled, signInWithPat, type DevSignInResult } from '@/auth/devSignIn';
import { signInWithWorkspace, type NativeSignInError } from '@/auth/nativeAuth';
import { identity } from '@/config/identity';
import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { useSession } from '@/state/session';
import { leaveSignIn } from '@/state/workspace';
import { refreshWorkspaceInfo, useWorkspaceInfo } from '@/state/workspaceInfo';
import { BrandLogo } from '@/theme/BrandLogo';
import { useBrandFontFamily } from '@/theme/brandFonts';
import { useBrandTheme } from '@/theme/brandStore';

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
  const fontFamily = useBrandFontFamily();

  return (
    <SafeAreaView style={[styles.flex, { backgroundColor: palette.background }]}>
      <KeyboardAvoidingView
        style={styles.flex}
        behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        <ScrollView contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
          <BrandLogo image={brand?.logoMark ?? brand?.logoFull ?? null} size={56} />
          <Text accessibilityRole="header" style={[styles.title, { color: palette.text, fontFamily }]}>
            Sign in
          </Text>
          <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
            <Text style={[styles.label, { color: palette.textSecondary }]}>Workspace</Text>
            <Text selectable style={[styles.host, { color: palette.text }]}>
              {serverOrigin ? displayHost(serverOrigin) : ''}
            </Text>
          </View>

          <NativeSignIn />

          {enabled ? <DevPatForm /> : null}

          <Pressable
            accessibilityRole="button"
            onPress={() => void leaveSignIn()}
            style={styles.linkButton}>
            <Text style={[styles.link, { color: palette.accent }]}>Use a different workspace</Text>
          </Pressable>
        </ScrollView>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}

function NativeSignIn() {
  const palette = usePalette();
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
        <Text style={[styles.body, { color: palette.textSecondary }]}>
          {loading ? 'Checking the workspace…' : "The workspace's sign-in details are not loaded."}
        </Text>
        <Pressable
          accessibilityRole="button"
          disabled={loading}
          onPress={() => void load()}
          style={[styles.button, styles.outline, { borderColor: palette.accent }]}>
          {loading ? (
            <ActivityIndicator color={palette.accent} />
          ) : (
            <Text style={[styles.buttonText, { color: palette.accent }]}>Try again</Text>
          )}
        </Pressable>
      </View>
    );
  }

  const assessment = assessDiscovery(current.discovery);
  if (assessment.kind === 'native_auth_unavailable' || assessment.kind === 'contract_unsupported') {
    return (
      <Text accessibilityRole="alert" style={[styles.body, { color: palette.textSecondary }]}>
        {assessment.kind === 'native_auth_unavailable'
          ? `This workspace hasn't enabled ${identity.displayName} sign-in. Ask your administrator to register the app.`
          : `This workspace needs a different version of ${identity.displayName}.`}
      </Text>
    );
  }

  const name = current.discovery.display_name || displayHost(current.origin);
  return (
    <View style={styles.form}>
      <Pressable
        accessibilityRole="button"
        testID="native-sign-in"
        accessibilityState={{ disabled: busy, busy }}
        disabled={busy}
        onPress={() => void onSignIn()}
        style={[styles.button, { backgroundColor: palette.accent, opacity: busy ? 0.6 : 1 }]}>
        {busy ? (
          <ActivityIndicator color={palette.onAccent} />
        ) : (
          <Text style={[styles.buttonText, { color: palette.onAccent }]}>Sign in with {name}</Text>
        )}
      </Pressable>
      <Text style={[styles.small, { color: palette.textSecondary }]}>
        Opens your organization&apos;s sign-in page in the system browser. {identity.displayName} never
        sees your password.
      </Text>
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
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
        <Text style={[styles.bannerText, { color: palette.danger }]}>Developer sign-in</Text>
        <Text style={[styles.small, { color: palette.textSecondary }]}>
          Paste a personal access token from your workspace profile. It is stored only on this
          device and grants full access to your account.
        </Text>
      </View>
      <Text style={[styles.label, { color: palette.textSecondary }]}>Personal access token</Text>
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
            backgroundColor: palette.backgroundElement,
            borderColor: error ? palette.danger : palette.border,
          },
        ]}
      />
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ disabled: busy, busy }}
        disabled={busy}
        onPress={() => void onSubmit()}
        style={[styles.button, { backgroundColor: palette.accent, opacity: busy ? 0.6 : 1 }]}>
        {busy ? (
          <ActivityIndicator color={palette.onAccent} />
        ) : (
          <Text style={[styles.buttonText, { color: palette.onAccent }]}>Sign in</Text>
        )}
      </Pressable>
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
  title: { fontSize: 28, fontWeight: '700', marginTop: Spacing.five },
  card: { borderRadius: 12, padding: Spacing.three, gap: Spacing.one },
  label: { fontSize: 13, fontWeight: '600' },
  host: { fontSize: 22, fontWeight: '800' },
  outline: { borderWidth: 1.5 },
  body: { fontSize: 16, lineHeight: 22 },
  form: { gap: Spacing.three },
  banner: { borderWidth: 1, borderRadius: 10, padding: Spacing.three, gap: Spacing.one },
  bannerText: { fontSize: 14, fontWeight: '700' },
  small: { fontSize: 13, lineHeight: 18 },
  input: {
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.three - 4,
    fontSize: 16,
  },
  error: { fontSize: 14 },
  button: { borderRadius: 10, paddingVertical: Spacing.three - 2, alignItems: 'center' },
  buttonText: { fontSize: 16, fontWeight: '600' },
  linkButton: { alignItems: 'center', paddingVertical: Spacing.two },
  link: { fontSize: 15 },
});
