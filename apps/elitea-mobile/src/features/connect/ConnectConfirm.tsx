import * as WebBrowser from 'expo-web-browser';
import { StyleSheet, View } from 'react-native';

import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';
import { canConnect, displayHost, type ConnectCheck } from '@/features/connect/connectFlow';
import { HostText } from '@/features/connect/HostText';
import { EntryScreen } from '@/features/entry/EntryScreen';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { BrandLogo } from '@/theme/BrandLogo';
import { Button, Icon, ListRow, ListSection, Text } from '@/ui';

/**
 * The connect confirmation (ADR-0025 WP1, docs/screens.md §1b). The workspace's identity is the
 * hero (its logo, else a monogram tile; its product name), and the host is the largest text on
 * the screen: it is the one thing a phishing link cannot fake, so the user checks it before
 * anything is stored or any sign-in page opens. The workspace's accent is NOT applied yet (the
 * buttons are Zefir's), so a workspace cannot style its own trust decision.
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
  const { colors, accessibilityLayout } = useDesign();
  const allowed = canConnect(check, devSignIn);
  const notice = noticeFor(check, devSignIn);
  const host = displayHost(check.origin);
  const secure = check.origin.startsWith('https://');
  const logo = check.kind === 'confirm' && check.brand ? (check.brand.logoMark ?? check.brand.logoFull) : null;
  const productName = check.kind === 'confirm' && check.brand && !check.brand.productDefault ? check.brand.productName : null;

  return (
    <EntryScreen
      testID="connect-confirm-screen"
      // At accessibility sizes the rows stack and fill the screen: Connect stays on screen (EH-5).
      pinFooter={accessibilityLayout}
      footer={
        <>
          {check.kind === 'failed' && check.canonicalOrigin ? (
            <Button
              variant={allowed ? 'tonal' : 'filled'}
              title={`Use ${displayHost(check.canonicalOrigin)}`}
              onPress={() => onUseCanonical(check.canonicalOrigin!)}
            />
          ) : null}
          {check.kind === 'confirm' && check.assessment.kind === 'upgrade_required' ? (
            <Button title="Get the update" onPress={() => void WebBrowser.openBrowserAsync(identity.webUrl)} />
          ) : null}
          {allowed ? <Button testID="connect-confirm" title="Connect" loading={busy} onPress={onConfirm} /> : null}
          <Button title="Use a different address" variant="plain" onPress={onBack} />
        </>
      }>
      {/* No mascot here: the workspace's identity is the hero of the trust decision (§3.5, §5). */}
      <View style={styles.identity}>
        <View style={[styles.logoTile, { backgroundColor: colors.surface, borderColor: colors.separator }]}>
          {logo ? <BrandLogo image={logo} size={56} /> : <WorkspaceMark origin={check.origin} image={null} size={56} />}
        </View>
        {productName ? (
          <Text variant="title3" emphasized style={styles.center}>
            {productName}
          </Text>
        ) : null}
        <Text variant="subheadline" tone="secondary" style={styles.center}>
          You are connecting to
        </Text>
        {/* The host is always in the system font: a brand font must not be able to disguise it. */}
        <HostText testID="connect-host" heading host={host} role="title2" color={colors.textPrimary} />
        {/* The full origin stays selectable (copy for support). */}
        <Text variant="footnote" tone="secondary" selectable style={styles.center}>
          {check.origin}
        </Text>
      </View>

      {check.kind === 'confirm' ? (
        <ListSection footer="Only connect to workspaces you trust. You can remove a workspace and its data at any time.">
          <ListRow
            icon={secure ? 'lock' : 'warning'}
            title={secure ? 'Secure connection' : 'Not encrypted'}
            value={secure ? 'HTTPS' : 'HTTP'}
          />
          <ListRow icon="workspace" title="Workspace" value={check.document.display_name || '—'} />
          <ListRow icon="info" title="Operated as" value={check.document.deployment_kind === 'saas' ? 'Hosted service' : 'Self-hosted'} />
          <ListRow icon="sync" title="Server version" value={check.document.server_version} />
        </ListSection>
      ) : null}

      {notice ? (
        <View style={[styles.notice, { backgroundColor: notice.tone === 'error' ? colors.dangerSoft : colors.surface }]}>
          <Icon name={notice.tone === 'error' ? 'error' : 'info'} size={20} tone={notice.tone === 'error' ? 'danger' : 'secondary'} />
          <Text accessibilityRole="alert" testID="connect-notice" variant="subheadline" style={styles.flex}>
            {notice.text}
          </Text>
        </View>
      ) : null}
    </EntryScreen>
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
  identity: { alignItems: 'center', gap: 6, marginTop: 16 },
  logoTile: {
    width: 80,
    height: 80,
    borderRadius: 22,
    borderCurve: 'continuous',
    borderWidth: StyleSheet.hairlineWidth,
    alignItems: 'center',
    justifyContent: 'center',
    marginBottom: 6,
  },
  center: { textAlign: 'center' },
  notice: { flexDirection: 'row', gap: 10, padding: 14, borderRadius: 16, borderCurve: 'continuous', alignItems: 'flex-start' },
  flex: { flex: 1 },
});
