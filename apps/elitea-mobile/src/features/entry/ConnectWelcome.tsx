import { useState, type ReactNode } from 'react';
import { StyleSheet, useWindowDimensions, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';
import { EntryScreen } from '@/features/entry/EntryScreen';
import { Button, Icon, Text, TextField } from '@/ui';

export interface ConnectWelcomeProps {
  /** Other workspaces are connected on this device: "Add a workspace" instead of the welcome. */
  adding: boolean;
  /** "Back to {host}" (Add workspace was opened from a workspace that is still connected). */
  backTo?: { host: string; onPress: () => void } | null;
  /** Why the last session ended when the user did not end it (revoked, rejected, policy). */
  endNotice?: string | null;
  address: string;
  onAddressChange: (text: string) => void;
  error: string | null;
  /** "Suggested by your organization." (MDM `server_url`). */
  managedHint: boolean;
  busy: boolean;
  onContinue: () => void;
  onPrivacy: () => void;
  onHelp: () => void;
  /** The connected workspaces (switching to one wipes nothing). */
  saved?: ReactNode;
}

/**
 * The connect screen (docs/screens.md §1a): Agent Zefirca on her pedestal, "Meet Agent Zefir",
 * one sentence, and ONE primary action. The address field opens in place ("Enter workspace
 * address"), already open when the address is known (a link, the last workspace, MDM). The
 * palette is always Zefir's here: no brand applies before the user confirms a workspace.
 *
 * No QR button: a prominent control that does nothing breaks App Review 2.1, so it comes back
 * with the scanner.
 */
export function ConnectWelcome({
  adding,
  backTo,
  endNotice,
  address,
  onAddressChange,
  error,
  managedHint,
  busy,
  onContinue,
  onPrivacy,
  onHelp,
  saved,
}: ConnectWelcomeProps) {
  const design = useDesign();
  const [entering, setEntering] = useState(address !== '' || error !== null);
  const open = entering || address !== '' || error !== null;
  // The hero shrinks to the bust at accessibility sizes and while typing, so the field and
  // its button stay above the keyboard.
  const compact = design.accessibilityLayout || open;
  // §5: the hero is 240 pt wide, 180 on short screens (SE-class heights).
  const { height } = useWindowDimensions();
  const heroWidth = height < 740 ? 180 : 240;
  return (
    <EntryScreen
      testID="connect-screen"
      footer={
        <>
          <Text variant="footnote" tone="secondary" style={styles.center}>
            You sign in on your organization&apos;s own page in a secure browser window. {identity.displayName} never sees your
            password.
          </Text>
          <View style={styles.links}>
            <Button title="Privacy" variant="plain" size="small" onPress={onPrivacy} />
            <Text variant="footnote" tone="tertiary" importantForAccessibility="no" accessibilityElementsHidden>
              ·
            </Text>
            <Button title="Help" variant="plain" size="small" onPress={onHelp} />
          </View>
        </>
      }>
      {backTo ? (
        <Button
          testID="connect-back"
          variant="plain"
          size="small"
          icon="back"
          title={`Back to ${backTo.host}`}
          onPress={backTo.onPress}
        />
      ) : null}
      <View style={styles.hero}>
        {adding ? null : compact ? (
          <Mascot variant="bust" size={96} decorative />
        ) : (
          <Mascot variant="hero" size={heroWidth} decorative />
        )}
        <Text variant="title1" heading style={styles.center}>
          {adding ? 'Add a workspace' : `Meet ${identity.displayName}`}
        </Text>
        <Text variant="callout" tone="secondary" style={styles.center}>
          {adding ? 'Your other workspaces stay connected and keep their data.' : "Your workspace's AI agents, in your pocket."}
        </Text>
      </View>

      {endNotice ? (
        <View testID="connect-end-reason" style={[styles.notice, { backgroundColor: design.colors.dangerSoft, borderRadius: design.radius.md }]}>
          <Icon name="warning" size={20} tone="danger" />
          <Text accessibilityRole="alert" variant="subheadline" style={styles.flex}>
            {endNotice}
          </Text>
        </View>
      ) : null}

      {open ? (
        <View style={styles.form}>
          <TextField
            label="Workspace address"
            value={address}
            onChangeText={onAddressChange}
            onSubmitEditing={onContinue}
            placeholder="acme.elitea.ai"
            autoCapitalize="none"
            autoCorrect={false}
            autoComplete="url"
            keyboardType="url"
            textContentType="URL"
            returnKeyType="go"
            autoFocus={entering && address === ''}
            error={error}
            note={managedHint ? 'Suggested by your organization.' : null}
            noteTestID="connect-managed-hint"
          />
          <Button testID="connect-continue" title="Continue" icon="arrowGo" loading={busy} disabled={address.trim() === ''} onPress={onContinue} />
        </View>
      ) : (
        <Button testID="connect-enter-address" title="Enter workspace address" icon="link" onPress={() => setEntering(true)} />
      )}

      {saved ?? null}
    </EntryScreen>
  );
}

const styles = StyleSheet.create({
  hero: { alignItems: 'center', gap: 8, marginTop: 8 },
  center: { textAlign: 'center' },
  notice: { flexDirection: 'row', gap: 10, padding: 14, alignItems: 'flex-start' },
  flex: { flex: 1 },
  form: { gap: 14 },
  links: { flexDirection: 'row', justifyContent: 'center', alignItems: 'center', gap: 4 },
});
