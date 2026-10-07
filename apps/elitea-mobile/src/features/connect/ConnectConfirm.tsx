import { Linking, StyleSheet, View } from 'react-native';

import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { canConnect, displayHost, type ConnectCheck } from '@/features/connect/connectFlow';
import { HostText } from '@/features/connect/HostText';
import { BrandLogo } from '@/theme/BrandLogo';
import { Button, Text } from '@/ui';

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
            <Text variant="title3" emphasized>
              {check.brand.productName}
            </Text>
          ) : null}
        </View>
      ) : null}
      {/* The host is always in the system font: a brand font must not be able to disguise it. */}
      <Text variant="subheadline" tone="secondary" style={styles.caption}>
        You are connecting to
      </Text>
      <HostText testID="connect-host" heading host={displayHost(check.origin)} role="title1" color={palette.text} />
      {/* The full origin stays selectable (copy for support). */}
      <Text variant="footnote" tone="secondary" selectable>
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
          variant="subheadline"
          tone={notice.tone === 'error' ? 'danger' : 'secondary'}>
          {notice.text}
        </Text>
      ) : null}

      {check.kind === 'failed' && check.canonicalOrigin ? (
        <Button
          variant="tonal"
          title={`Use ${displayHost(check.canonicalOrigin)}`}
          onPress={() => onUseCanonical(check.canonicalOrigin!)}
        />
      ) : null}

      {check.kind === 'confirm' && check.assessment.kind === 'upgrade_required' ? (
        <Button title="Get the update" onPress={() => void Linking.openURL(identity.webUrl)} />
      ) : null}

      {allowed ? (
        <Button testID="connect-confirm" title="Connect" loading={busy} onPress={onConfirm} />
      ) : null}

      <Button title="Use a different address" variant="plain" onPress={onBack} />
    </View>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.row}>
      <Text variant="subheadline" tone="secondary" emphasized>
        {label}
      </Text>
      <Text variant="subheadline" style={styles.value}>
        {value}
      </Text>
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
  caption: { marginTop: Spacing.four },
  card: { borderRadius: 16, borderCurve: 'continuous', padding: Spacing.three, gap: Spacing.two },
  row: { flexDirection: 'row', justifyContent: 'space-between', gap: Spacing.three },
  value: { flexShrink: 1, textAlign: 'right' },
});
