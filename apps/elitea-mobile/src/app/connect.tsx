import { useLocalSearchParams } from 'expo-router';
import { useEffect, useState } from 'react';
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

import { devSignInEnabled } from '@/auth/devSignIn';
import { identity } from '@/config/identity';
import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { ConnectConfirm } from '@/features/connect/ConnectConfirm';
import { checkWorkspace, confirmConnection, displayHost, type ConnectCheck } from '@/features/connect/connectFlow';
import { WorkspaceRow } from '@/features/workspaces/WorkspaceRow';
import { parseConnectLink, type ConnectLinkErrorCode } from '@/linking/parseConnectLink';
import { useManagedConfig } from '@/security/managedConfig';
import { MAX_WORKSPACES } from '@/security/workspaceRegistry';
import { setSession, useSession, type EndReason } from '@/state/session';
import { returnToPreviousWorkspace, switchToWorkspace } from '@/state/workspace';
import { useWorkspaceList } from '@/state/workspaceList';

const ERROR_TEXT: Record<ConnectLinkErrorCode, string> = {
  empty: 'Enter your workspace address.',
  unsupported_scheme: 'That is not a workspace address or connect link.',
  not_connect_link: 'That link is not a connect link.',
  missing_server: 'The connect link does not contain a server address.',
  ambiguous_server: 'The connect link contains more than one server address.',
  nested_link: 'The connect link does not point to a web address.',
  insecure_http: 'The workspace must use a secure (https) address.',
  credentials_in_url: 'Remove the username or password from the address.',
  invalid_host: 'That server address is not valid.',
  invalid_port: 'That port number is not valid.',
  malformed: 'That address could not be read.',
};

/**
 * Why the last session ended, when the user did not end it. Every end of a
 * session removes that workspace, so this screen (not sign-in) is where it
 * lands, naming the workspace; connecting again clears `endReason`. Other
 * connected workspaces are listed below and were not affected.
 */
const END_REASON_TEXT: Partial<Record<EndReason, (host: string) => string>> = {
  device_revoked: (host) => `This device was signed out by the workspace ${host}. Its data was removed from this device.`,
  credential_rejected: (host) => `The workspace ${host} no longer accepts your sign-in. Connect and sign in again.`,
  policy: (host) => `The security policy of ${host} signed this device out.`,
};

export default function ConnectScreen() {
  const palette = usePalette();
  // Deep links `agentzefir://connect?server=...` land here with `server` as a search param.
  const params = useLocalSearchParams<{ server?: string }>();
  const incoming = typeof params.server === 'string' ? params.server : '';
  // After a rejected credential the last origin is kept (in memory only) as a hint.
  const lastOriginHint = useSession((s) => s.lastOriginHint);
  const endReason = useSession((s) => s.endReason);
  const endedOrigin = useSession((s) => s.endedOrigin);
  const endNotice =
    endReason === null ? undefined : END_REASON_TEXT[endReason]?.(endedOrigin ? displayHost(endedOrigin) : 'you were using');
  // The other workspaces connected on this device (switching to one wipes nothing).
  const workspaces = useWorkspaceList();
  const returnTo = useSession((s) => s.returnWorkspaceId);
  const returnHost = workspaces.find((w) => w.id === returnTo);
  // MDM AppConfig `server_url` (src/security/managedConfig.ts) pre-fills an empty field.
  const managedServer = useManagedConfig().serverUrl;
  const [address, setAddress] = useState(incoming || lastOriginHint || managedServer || '');
  const [error, setError] = useState<string | null>(null);
  // Discovery result awaiting the user's confirmation; nothing is stored yet.
  const [check, setCheck] = useState<ConnectCheck | null>(null);

  // A new deep link while this screen is mounted replaces the field contents
  // (adjust-state-during-render pattern rather than an effect).
  const [lastIncoming, setLastIncoming] = useState(incoming);
  if (incoming !== lastIncoming) {
    setLastIncoming(incoming);
    if (incoming !== '') {
      setAddress(incoming);
      setError(null);
      setCheck(null);
    }
  }

  const [busy, setBusy] = useState(false);
  const devSignIn = devSignInEnabled();

  const runCheck = async (origin: string) => {
    setBusy(true);
    const result = await checkWorkspace(origin);
    setBusy(false);
    setCheck(result);
  };

  // A workspace switch (connect link while connected) lands here with the new
  // origin: check it and show the confirmation straight away.
  const pendingOrigin = useSession((s) => s.pendingConnectOrigin);
  useEffect(() => {
    if (pendingOrigin === null) return;
    let cancelled = false;
    void checkWorkspace(pendingOrigin).then((result) => {
      if (cancelled) return;
      setAddress(pendingOrigin);
      setError(null);
      setCheck(result);
      // Consumed only once shown (clearing it earlier would cancel this effect).
      setSession({ pendingConnectOrigin: null });
    });
    return () => {
      cancelled = true;
    };
  }, [pendingOrigin]);

  const onContinue = async () => {
    if (busy) return;
    const result = parseConnectLink(address, {
      connectSchemes: identity.connectSchemes,
      allowPrivateHttp: __DEV__, // dev-only: plain http to localhost / 10.x / 192.168.x
    });
    if (!result.ok) {
      setError(ERROR_TEXT[result.error]);
      return;
    }
    setError(null);
    await runCheck(result.origin);
  };

  const onConfirm = async () => {
    if (busy || check === null) return;
    setBusy(true);
    const stored = await confirmConnection(check);
    setBusy(false);
    // On success the root layout's guards route to /sign-in.
    if (!stored) {
      setCheck(null);
      setError('The workspace address could not be saved on this device. Try again.');
    }
  };

  if (check !== null) {
    return (
      <SafeAreaView style={[styles.flex, { backgroundColor: palette.background }]}>
        <ScrollView contentContainerStyle={styles.content}>
          <ConnectConfirm
            check={check}
            devSignIn={devSignIn}
            busy={busy}
            onConfirm={() => void onConfirm()}
            onBack={() => setCheck(null)}
            onUseCanonical={(origin) => {
              setAddress(origin);
              void runCheck(origin);
            }}
          />
        </ScrollView>
      </SafeAreaView>
    );
  }

  const onScanQr = () => {
    // TODO: camera QR scanner (expo-camera); feed the payload through parseConnectLink.
    setError('QR scanning is not available yet. Type the address instead.');
  };

  return (
    <SafeAreaView style={[styles.flex, { backgroundColor: palette.background }]}>
      <KeyboardAvoidingView
        style={styles.flex}
        behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        <ScrollView contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
          {returnHost ? (
            <Pressable
              accessibilityRole="button"
              testID="connect-back"
              onPress={() => void returnToPreviousWorkspace()}
              style={styles.back}>
              <Text style={[styles.link, { color: palette.accent }]}>‹ Back to {displayHost(returnHost.origin)}</Text>
            </Pressable>
          ) : null}
          <Text style={[styles.title, { color: palette.text }]}>
            {workspaces.length > 0 ? 'Add a workspace' : 'Connect to your workspace'}
          </Text>
          {endNotice ? (
            <Text
              accessibilityRole="alert"
              testID="connect-end-reason"
              style={[styles.notice, { color: palette.danger, borderColor: palette.danger }]}>
              {endNotice}
            </Text>
          ) : null}
          <Text style={[styles.body, { color: palette.textSecondary }]}>
            {identity.displayName} connects to your organization&apos;s AI agent workspace. Scan
            the QR code from your workspace&apos;s settings page, open a connect link, or type the
            address you use in your browser.
          </Text>

          <Pressable
            accessibilityRole="button"
            onPress={onScanQr}
            style={[styles.button, { backgroundColor: palette.accent }]}>
            <Text style={[styles.buttonText, { color: palette.onAccent }]}>Scan QR code</Text>
          </Pressable>

          <Text style={[styles.label, { color: palette.textSecondary }]}>Workspace address</Text>
          <TextInput
            value={address}
            onChangeText={(text) => {
              setAddress(text);
              setError(null);
            }}
            onSubmitEditing={() => void onContinue()}
            placeholder="https://ai.example.com"
            placeholderTextColor={palette.textSecondary}
            autoCapitalize="none"
            autoCorrect={false}
            autoComplete="url"
            keyboardType="url"
            textContentType="URL"
            returnKeyType="go"
            accessibilityLabel="Workspace address"
            style={[
              styles.input,
              {
                color: palette.text,
                backgroundColor: palette.backgroundElement,
                borderColor: error ? palette.danger : palette.border,
              },
            ]}
          />
          {managedServer !== null && address.trim() === managedServer && !error ? (
            <Text testID="connect-managed-hint" style={[styles.error, { color: palette.textSecondary }]}>
              Suggested by your organization.
            </Text>
          ) : null}
          {error ? (
            <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
              {error}
            </Text>
          ) : null}

          <Pressable
            accessibilityRole="button"
            onPress={() => void onContinue()}
            disabled={busy}
            accessibilityState={{ disabled: busy, busy }}
            style={[styles.button, styles.secondary, { borderColor: palette.accent }]}>
            {busy ? (
              <ActivityIndicator color={palette.accent} />
            ) : (
              <Text style={[styles.buttonText, { color: palette.accent }]}>Continue</Text>
            )}
          </Pressable>

          <View style={styles.footer}>
            <Text style={[styles.small, { color: palette.textSecondary }]}>
              You will sign in with your organization&apos;s usual login page in the system
              browser. {identity.displayName} never sees your password.
            </Text>
          </View>

          {workspaces.length > 0 ? (
            <View style={styles.saved}>
              <Text accessibilityRole="header" style={[styles.label, { color: palette.textSecondary }]}>
                Your workspaces on this device
              </Text>
              <View style={[styles.savedList, { backgroundColor: palette.backgroundElement }]}>
                {workspaces.map((workspace) => (
                  <WorkspaceRow
                    key={workspace.id}
                    workspace={workspace}
                    active={false}
                    onPress={() => void switchToWorkspace(workspace.id)}
                  />
                ))}
              </View>
              {workspaces.length >= MAX_WORKSPACES ? (
                <Text style={[styles.small, { color: palette.textSecondary }]}>
                  At most {MAX_WORKSPACES} workspaces can be connected. Open one and remove it from its
                  Workspaces list to add another.
                </Text>
              ) : null}
            </View>
          ) : null}
        </ScrollView>
      </KeyboardAvoidingView>
    </SafeAreaView>
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
  title: { fontSize: 28, fontWeight: '700', marginTop: Spacing.four },
  body: { fontSize: 16, lineHeight: 22 },
  label: { fontSize: 13, fontWeight: '600', marginTop: Spacing.three },
  input: {
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.three - 4,
    fontSize: 16,
  },
  error: { fontSize: 14 },
  notice: {
    fontSize: 15,
    lineHeight: 21,
    borderWidth: 1,
    borderRadius: 10,
    padding: Spacing.three,
  },
  button: {
    borderRadius: 10,
    paddingVertical: Spacing.three - 2,
    alignItems: 'center',
  },
  secondary: { borderWidth: 1.5 },
  buttonText: { fontSize: 16, fontWeight: '600' },
  footer: { marginTop: Spacing.four },
  back: { marginTop: Spacing.three, alignSelf: 'flex-start' },
  link: { fontSize: 16 },
  saved: { marginTop: Spacing.four, gap: Spacing.two },
  savedList: { borderRadius: 12 },
  small: { fontSize: 13, lineHeight: 18 },
});
