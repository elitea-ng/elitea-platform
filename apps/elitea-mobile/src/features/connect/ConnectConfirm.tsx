import { Linking, Pressable, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { canConnect, displayHost, type ConnectCheck } from '@/features/connect/connectFlow';
import { BrandLogo } from '@/theme/BrandLogo';

/**
 * The connect confirmation (ADR-0025 WP1). The host is the largest text on
 * the screen: it is the one thing a phishing link cannot fake, so the user
 * checks it before anything is stored or any sign-in page opens.
 */
export function ConnectConfirm({
  check,
  devSignIn,
  busy,
  onConfirm,
  onBack,
  onUseCanonical,
}: {
  check: ConnectCheck;
  devSignIn: boolean;
  busy: boolean;
  onConfirm: () => void;
  onBack: () => void;
  onUseCanonical: (origin: string) => void;
}) {
  const palette = usePalette();
  const allowed = canConnect(check, devSignIn);
  const notice = noticeFor(check, devSignIn);

  return (
    <View style={styles.container}>
      {check.kind === 'confirm' && check.brand ? (
        <View style={styles.brand}>
          <BrandLogo image={check.brand.logoMark ?? check.brand.logoFull} size={64} />
          {check.brand.productName ? (
            <Text style={[styles.brandName, { color: palette.text }]}>{check.brand.productName}</Text>
          ) : null}
        </View>
      ) : null}
      {/* The host is always in the system font: a brand font must not be able to disguise it. */}
      <Text style={[styles.caption, { color: palette.textSecondary }]}>You are connecting to</Text>
      <Text
        accessibilityRole="header"
        testID="connect-host"
        selectable
        style={[styles.host, { color: palette.text }]}>
        {displayHost(check.origin)}
      </Text>
      <Text selectable style={[styles.origin, { color: palette.textSecondary }]}>
        {check.origin}
      </Text>

      {check.kind === 'confirm' ? (
        <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
          <Row label="Workspace" value={check.document.display_name || '—'} />
          <Row
            label="Operated as"
            value={check.document.deployment_kind === 'saas' ? 'Hosted service' : 'Self-hosted'}
          />
          <Row label="Server version" value={check.document.server_version} />
        </View>
      ) : null}

      {notice ? (
        <Text
          accessibilityRole="alert"
          testID="connect-notice"
          style={[styles.notice, { color: notice.tone === 'error' ? palette.danger : palette.textSecondary }]}>
          {notice.text}
        </Text>
      ) : null}

      {check.kind === 'failed' && check.canonicalOrigin ? (
        <Pressable
          accessibilityRole="button"
          onPress={() => onUseCanonical(check.canonicalOrigin!)}
          style={[styles.button, styles.outline, { borderColor: palette.accent }]}>
          <Text style={[styles.buttonText, { color: palette.accent }]}>
            Use {displayHost(check.canonicalOrigin)}
          </Text>
        </Pressable>
      ) : null}

      {check.kind === 'confirm' && check.assessment.kind === 'upgrade_required' ? (
        <Pressable
          accessibilityRole="button"
          onPress={() => void Linking.openURL(identity.webUrl)}
          style={[styles.button, { backgroundColor: palette.accent }]}>
          <Text style={[styles.buttonText, { color: palette.onAccent }]}>Get the update</Text>
        </Pressable>
      ) : null}

      {allowed ? (
        <Pressable
          accessibilityRole="button"
          testID="connect-confirm"
          disabled={busy}
          accessibilityState={{ disabled: busy, busy }}
          onPress={onConfirm}
          style={[styles.button, { backgroundColor: palette.accent, opacity: busy ? 0.6 : 1 }]}>
          <Text style={[styles.buttonText, { color: palette.onAccent }]}>Connect</Text>
        </Pressable>
      ) : null}

      <Pressable accessibilityRole="button" onPress={onBack} style={styles.linkButton}>
        <Text style={[styles.link, { color: palette.accent }]}>Use a different address</Text>
      </Pressable>
    </View>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  const palette = usePalette();
  return (
    <View style={styles.row}>
      <Text style={[styles.label, { color: palette.textSecondary }]}>{label}</Text>
      <Text style={[styles.value, { color: palette.text }]}>{value}</Text>
    </View>
  );
}

export function noticeFor(
  check: ConnectCheck,
  devSignIn: boolean,
): { tone: 'error' | 'info'; text: string } | null {
  const app = identity.displayName;
  if (check.kind === 'failed') {
    switch (check.error) {
      case 'not_supported':
        return {
          tone: devSignIn ? 'info' : 'error',
          text: devSignIn
            ? `This server does not publish ${app} support. You can still connect with developer sign-in.`
            : `This server does not support ${app}. Check the address, or ask your administrator.`,
        };
      case 'unavailable':
        return { tone: 'error', text: 'The workspace is temporarily unavailable. Try again in a moment.' };
      case 'unreachable':
        return { tone: 'error', text: "Can't reach that server. Check the address and your connection." };
      case 'invalid':
        return { tone: 'error', text: 'That server answered, but not like an AI agent workspace.' };
      case 'origin_mismatch':
        return {
          tone: 'error',
          text: check.canonicalOrigin
            ? `This workspace is published at ${displayHost(check.canonicalOrigin)}. Connect to that address instead.`
            : 'This workspace points to a different server. Ask your administrator for the right address.',
        };
    }
  }
  switch (check.assessment.kind) {
    case 'ready':
      return null;
    case 'native_auth_unavailable':
      return {
        tone: devSignIn ? 'info' : 'error',
        text:
          `This workspace hasn't enabled ${app} sign-in. Ask your administrator to register the app.` +
          (devSignIn ? ' Developer sign-in is still available.' : ''),
      };
    case 'contract_unsupported':
      return {
        tone: 'error',
        text: `This workspace speaks a version of the client contract (${check.assessment.serverContract}) that this version of ${app} does not support.`,
      };
    case 'upgrade_required':
      return {
        tone: 'error',
        text: `This workspace needs ${app} ${check.assessment.minVersion} or later. Update the app to connect.`,
      };
  }
}

const styles = StyleSheet.create({
  container: { gap: Spacing.three },
  brand: { alignItems: 'center', gap: Spacing.two, marginTop: Spacing.four },
  brandName: { fontSize: 18, fontWeight: '700' },
  caption: { fontSize: 15, marginTop: Spacing.four },
  host: { fontSize: 30, fontWeight: '800' },
  origin: { fontSize: 13 },
  card: { borderRadius: 12, padding: Spacing.three, gap: Spacing.two },
  row: { flexDirection: 'row', justifyContent: 'space-between', gap: Spacing.three },
  label: { fontSize: 14, fontWeight: '600' },
  value: { fontSize: 14, flexShrink: 1, textAlign: 'right' },
  notice: { fontSize: 15, lineHeight: 21 },
  button: { borderRadius: 10, paddingVertical: Spacing.three - 2, alignItems: 'center' },
  outline: { borderWidth: 1.5 },
  buttonText: { fontSize: 16, fontWeight: '600' },
  linkButton: { alignItems: 'center', paddingVertical: Spacing.two },
  link: { fontSize: 15 },
});
