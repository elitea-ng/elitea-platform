import { useLocalSearchParams } from 'expo-router';
import { useState } from 'react';
import {
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

import { identity } from '@/config/identity';
import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { parseConnectLink, type ConnectLinkErrorCode } from '@/linking/parseConnectLink';
import { useWorkspace } from '@/state/workspace';

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

export default function ConnectScreen() {
  const palette = usePalette();
  const { connect } = useWorkspace();
  // Deep links `agentzefir://connect?server=...` land here with `server` as a search param.
  const params = useLocalSearchParams<{ server?: string }>();
  const incoming = typeof params.server === 'string' ? params.server : '';
  const [address, setAddress] = useState(incoming);
  const [error, setError] = useState<string | null>(null);

  // A new deep link while this screen is mounted replaces the field contents
  // (adjust-state-during-render pattern rather than an effect).
  const [lastIncoming, setLastIncoming] = useState(incoming);
  if (incoming !== lastIncoming) {
    setLastIncoming(incoming);
    if (incoming !== '') {
      setAddress(incoming);
      setError(null);
    }
  }

  const onContinue = () => {
    const result = parseConnectLink(address, {
      connectSchemes: identity.connectSchemes,
      allowPrivateHttp: __DEV__, // dev-only: plain http to localhost / 10.x / 192.168.x
    });
    if (!result.ok) {
      setError(ERROR_TEXT[result.error]);
      return;
    }
    setError(null);
    // TODO: discover the server's native-client support + brand pack, then start
    // OAuth (code + PKCE, system browser) before treating the workspace as connected.
    connect(result.origin);
  };

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
          <Text style={[styles.title, { color: palette.text }]}>Connect to your workspace</Text>
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
            onSubmitEditing={onContinue}
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
          {error ? (
            <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
              {error}
            </Text>
          ) : null}

          <Pressable
            accessibilityRole="button"
            onPress={onContinue}
            style={[styles.button, styles.secondary, { borderColor: palette.accent }]}>
            <Text style={[styles.buttonText, { color: palette.accent }]}>Continue</Text>
          </Pressable>

          <View style={styles.footer}>
            <Text style={[styles.small, { color: palette.textSecondary }]}>
              You will sign in with your organization&apos;s usual login page in the system
              browser. {identity.displayName} never sees your password.
            </Text>
          </View>
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
  title: { fontSize: 28, fontWeight: '700', marginTop: Spacing.five },
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
  button: {
    borderRadius: 10,
    paddingVertical: Spacing.three - 2,
    alignItems: 'center',
  },
  secondary: { borderWidth: 1.5 },
  buttonText: { fontSize: 16, fontWeight: '600' },
  footer: { marginTop: Spacing.four },
  small: { fontSize: 13, lineHeight: 18 },
});
