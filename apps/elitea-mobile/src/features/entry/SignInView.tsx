import { useState, type ReactNode } from 'react';
import { StyleSheet, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { identity } from '@/config/identity';
import type { BrandInput } from '@/design/brand';
import { useDesign } from '@/design/DesignProvider';
import { HostText } from '@/features/connect/HostText';
import { EntryScreen } from '@/features/entry/EntryScreen';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { BrandLogo } from '@/theme/BrandLogo';
import type { BrandImage } from '@/theme/brandPack';
import { Button, Icon, Text } from '@/ui';

/** What the workspace's native sign-in can do right now. */
export type NativeSignInState =
  | { kind: 'ready'; busy: boolean; error: string | null; onSignIn: () => void }
  | { kind: 'loading' }
  | { kind: 'unloaded'; onRetry: () => void }
  | { kind: 'unavailable'; text: string };

export interface SignInViewProps {
  host: string;
  origin: string;
  /** The workspace's name (discovery `display_name`, else the host). */
  name: string;
  logo: BrandImage | null;
  brand: BrandInput | null;
  native: NativeSignInState;
  /** Development builds: the personal-access-token form, behind "Other sign-in options". */
  developer?: ReactNode;
  onUseDifferentWorkspace: () => void;
}

/**
 * Sign-in (docs/screens.md §1c): Agent Zefir and the workspace side by side in SEPARATE tiles
 * (the mascot never overlaps, frames or replaces the workspace logo, §5), "Sign in to {name}",
 * the host prominent in the system font, ONE filled button in the workspace's corrected accent
 * (the brand applies from here on), and where the sign-in happens. Developer sign-in is never on
 * the default path: it sits behind a disclosure, and exists in development builds only.
 */
export function SignInView({ host, origin, name, logo, brand, native, developer, onUseDifferentWorkspace }: SignInViewProps) {
  const { colors, radius } = useDesign();
  const [otherOpen, setOtherOpen] = useState(false);
  const tile = [styles.tile, { backgroundColor: colors.surface, borderColor: colors.separator, borderRadius: radius.lg }];
  return (
    <EntryScreen
      testID="sign-in-screen"
      footer={<Button title="Use a different workspace" variant="plain" onPress={onUseDifferentWorkspace} />}>
      <View style={styles.identity}>
        <View style={styles.tiles}>
          <View style={tile}>
            <Mascot variant="bust" size={64} decorative />
          </View>
          <View style={tile}>{logo ? <BrandLogo image={logo} size={48} /> : <WorkspaceMark origin={origin} image={null} brand={brand} size={48} />}</View>
        </View>
        <Text variant="title2" heading style={styles.center}>
          Sign in to {name}
        </Text>
        {/* The host stays prominent and in the system font: it is what the user is trusting. */}
        <View style={[styles.hostChip, { backgroundColor: colors.surface, borderColor: colors.separator }]}>
          <Icon name={origin.startsWith('https://') ? 'lock' : 'warning'} size={14} tone="secondary" />
          <View style={styles.shrink}>
            <HostText host={host} role="headline" color={colors.textPrimary} testID="sign-in-host" />
          </View>
        </View>
      </View>

      <View style={styles.form}>
        {native.kind === 'ready' ? (
          <>
            <Button testID="native-sign-in" title={native.busy ? 'Signing in…' : 'Continue with single sign-on'} icon="openExternal" loading={native.busy} onPress={native.onSignIn} />
            <Text variant="footnote" tone="secondary" style={styles.center}>
              You&apos;ll sign in on {host} in a secure browser window. {identity.displayName} never sees your password.
            </Text>
            {native.error ? (
              <View style={[styles.notice, { backgroundColor: colors.dangerSoft }]}>
                <Icon name="error" size={18} tone="danger" />
                <Text accessibilityRole="alert" variant="subheadline" style={styles.flex}>
                  {native.error}
                </Text>
              </View>
            ) : null}
          </>
        ) : native.kind === 'unavailable' ? (
          <View style={[styles.notice, { backgroundColor: colors.surface }]}>
            <Icon name="info" size={18} tone="secondary" />
            <Text accessibilityRole="alert" variant="subheadline" style={styles.flex}>
              {native.text}
            </Text>
          </View>
        ) : (
          <>
            <Text tone="secondary" style={styles.center}>
              {native.kind === 'loading' ? 'Checking the workspace…' : "The workspace's sign-in details are not loaded."}
            </Text>
            <Button
              title="Try again"
              variant="tonal"
              loading={native.kind === 'loading'}
              onPress={native.kind === 'unloaded' ? native.onRetry : undefined}
            />
          </>
        )}
      </View>

      {developer ? (
        <View style={styles.form}>
          <Button
            testID="other-sign-in"
            title="Other sign-in options"
            variant="plain"
            size="small"
            icon={otherOpen ? 'chevronDown' : 'chevronRight'}
            accessibilityState={{ expanded: otherOpen }}
            onPress={() => setOtherOpen((open) => !open)}
            style={styles.disclosure}
          />
          {otherOpen ? developer : null}
        </View>
      ) : null}
    </EntryScreen>
  );
}

const styles = StyleSheet.create({
  identity: { alignItems: 'center', gap: 12, marginTop: 24 },
  tiles: { flexDirection: 'row', gap: 16, marginBottom: 4 },
  tile: { width: 80, height: 80, alignItems: 'center', justifyContent: 'center', overflow: 'hidden', borderWidth: StyleSheet.hairlineWidth, borderCurve: 'continuous' },
  hostChip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    paddingHorizontal: 14,
    paddingVertical: 8,
    borderRadius: 9999,
    borderWidth: StyleSheet.hairlineWidth,
    maxWidth: '100%',
  },
  center: { textAlign: 'center' },
  form: { gap: 12 },
  notice: { flexDirection: 'row', gap: 10, padding: 14, borderRadius: 16, borderCurve: 'continuous', alignItems: 'flex-start' },
  flex: { flex: 1 },
  shrink: { flexShrink: 1 },
  disclosure: { alignSelf: 'center' },
});
