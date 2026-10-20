import type { ReactElement, ReactNode } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View, type RefreshControlProps, type ViewProps } from 'react-native';

import type { NativeDevice } from '@/api/queries/devices';
import { SwipeToDelete } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { Button, EmptyState, Icon, IconTile, ListSection, Text, useLayoutMargin } from '@/ui';

export interface DevicesViewProps {
  status: { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string };
  devices: readonly NativeDevice[];
  /** The workspace host the devices are signed in to (always shown, §3.5). */
  host: string;
  /** "now", "5m", "2d": the age of a timestamp. */
  age: (iso: string) => string;
  banner: string | null;
  /** Signs another device out after the user confirmed; resolves false when it did not happen. */
  onRevoke: (device: NativeDevice) => Promise<boolean>;
  onRetry: () => void;
  refreshControl?: ReactElement<RefreshControlProps>;
}

const PLATFORM_NAME: Record<NativeDevice['platform'], string> = { ios: 'iOS', android: 'Android', other: 'Other' };

/** "iOS · Agent Zefir 0.1.0". */
export function deviceMeta(device: Pick<NativeDevice, 'platform' | 'clientVersion'>): string {
  return [PLATFORM_NAME[device.platform], device.clientVersion ? `Agent Zefir ${device.clientVersion}` : null].filter(Boolean).join(' · ');
}

/**
 * Settings → Signed-in devices: this device first (no sign-out here: "Log out & wipe" in Settings
 * does that, with its wipe), then every other device signed in to the workspace with this account.
 * Each other device has a visible Sign out button; swiping the row left is the shortcut (and its
 * accessibility action, S9). The route confirms first. The
 * route and the `__DEV__` preview both render this (D2). No mascot: a security surface (§5).
 */
export function DevicesView({ status, devices, host, age, banner, onRevoke, onRetry, refreshControl }: DevicesViewProps) {
  const { colors, radius } = useDesign();
  const margin = useLayoutMargin();
  if (status.kind === 'loading') {
    return (
      <View style={[styles.flex, styles.center, { backgroundColor: colors.background }]}>
        <ActivityIndicator color={colors.accent} />
      </View>
    );
  }
  const current = devices.filter((d) => d.current);
  const others = devices.filter((d) => !d.current);
  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={[styles.content, { paddingHorizontal: margin }]}
      refreshControl={refreshControl}>
      {banner ? (
        <View accessible accessibilityRole="alert" style={[styles.banner, { backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="callout" tone="danger" style={styles.flex}>
            {banner}
          </Text>
        </View>
      ) : null}
      {status.kind === 'error' && devices.length === 0 ? (
        <EmptyState mascot="none" title="Couldn't load your devices" body={status.message} action={{ title: 'Try again', onPress: onRetry }} />
      ) : (
        <>
          {current.length > 0 ? (
            <ListSection title="This device">
              {current.map((device) => (
                <DeviceRow key={device.id} device={device} detail="Active now" />
              ))}
            </ListSection>
          ) : null}
          <ListSection
            title="Other devices"
            footer={
              others.length === 0
                ? `No other device is signed in to ${host} with your account.`
                : `Signing a device out ends its access to ${host} at once. The app on it erases this workspace's data when it next opens.`
            }>
            {others.map((device) => (
              <SwipeToDelete key={device.id} actionLabel="Sign out" onDelete={() => onRevoke(device)} testID={`device-${device.id}`}>
                {(a11y, remove) => (
                  <DeviceRow
                    device={device}
                    detail={device.lastSeenAt ? `Last active ${age(device.lastSeenAt)}` : null}
                    a11y={{
                      accessibilityActions: a11y.accessibilityActions,
                      onAccessibilityAction: a11y.onAccessibilityAction,
                      busy: a11y.accessibilityState?.busy,
                    }}
                    action={
                      <Button
                        title="Sign out"
                        variant="destructivePlain"
                        size="small"
                        accessibilityLabel={`Sign out ${device.name}`}
                        accessibilityHint="Asks first"
                        loading={a11y.accessibilityState?.busy === true}
                        onPress={remove}
                      />
                    }
                  />
                )}
              </SwipeToDelete>
            ))}
          </ListSection>
        </>
      )}
    </ScrollView>
  );
}

function DeviceRow({
  device,
  detail,
  a11y,
  action,
}: {
  device: NativeDevice;
  detail: string | null;
  a11y?: Pick<ViewProps, 'accessibilityActions' | 'onAccessibilityAction'> & { busy?: boolean };
  /** The row's own control (Sign out): outside the accessible group, so VoiceOver can focus it. */
  action?: ReactNode;
}) {
  const { colors, accessibilityLayout } = useDesign();
  const meta = deviceMeta(device);
  const tile = (
    <IconTile
      icon={device.platform === 'other' ? 'devices' : /ipad|tablet/i.test(device.name) ? 'tablet' : 'phone'}
      size={36}
      tone={device.current ? 'accent' : 'secondary'}
    />
  );
  const detailText = detail ? (
    <Text variant="footnote" tone={device.current ? 'success' : 'secondary'}>
      {detail}
    </Text>
  ) : null;
  return (
    <View
      testID={`device-${device.id}-body`}
      // At accessibility sizes every row stacks the same way (S3): the symbol, then the name, the
      // platform and version, and the last activity, each across the full width.
      style={[accessibilityLayout ? styles.stacked : styles.row, { backgroundColor: colors.surface }]}>
      <View
        accessible
        accessibilityLabel={[device.name, device.current ? 'this device' : null, meta, detail].filter(Boolean).join(', ')}
        accessibilityActions={a11y?.accessibilityActions}
        onAccessibilityAction={a11y?.onAccessibilityAction}
        accessibilityState={a11y?.busy ? { busy: true } : undefined}
        style={accessibilityLayout ? styles.stackedGroup : styles.group}>
        {tile}
        <View style={accessibilityLayout ? styles.stackedText : styles.text}>
          <Text variant="body">{device.name}</Text>
          <Text variant="footnote" tone="secondary">
            {meta}
          </Text>
          {/* Under the platform line at every size: the trailing column is the Sign out button's. */}
          {detailText}
        </View>
      </View>
      {action}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { alignItems: 'center', justifyContent: 'center' },
  content: { paddingVertical: 16, paddingBottom: 40, gap: 28 },
  banner: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12, borderCurve: 'continuous' },
  row: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 16, paddingVertical: 10, minHeight: 60 },
  text: { flex: 1, gap: 2 },
  group: { flex: 1, flexDirection: 'row', alignItems: 'center', gap: 12 },
  stacked: { gap: 8, paddingHorizontal: 16, paddingVertical: 12, alignItems: 'flex-start' },
  stackedGroup: { gap: 8, alignSelf: 'stretch' },
  stackedText: { gap: 2 },
});
