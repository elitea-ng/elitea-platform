import { Alert, Linking, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useWorkspace } from '@/state/workspace';

export default function SettingsScreen() {
  const palette = usePalette();
  const { serverOrigin, clear } = useWorkspace();

  const onLogOut = () => {
    Alert.alert(
      'Log out & wipe',
      'This signs you out and deletes all data stored on this device for this workspace.',
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Log out & wipe',
          style: 'destructive',
          // TODO: revoke tokens server-side, delete secure-store keys, the SQLCipher
          // database, encrypted attachments and the outbox — then clear().
          onPress: clear,
        },
      ],
    );
  };

  return (
    <ScrollView
      style={{ backgroundColor: palette.background }}
      contentContainerStyle={styles.content}>
      <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
        <Text style={[styles.label, { color: palette.textSecondary }]}>Workspace</Text>
        <Text selectable style={[styles.value, { color: palette.text }]}>
          {serverOrigin ?? 'Not connected'}
        </Text>
      </View>

      <Pressable
        accessibilityRole="button"
        onPress={onLogOut}
        style={[styles.button, { borderColor: palette.danger }]}>
        <Text style={[styles.buttonText, { color: palette.danger }]}>Log out & wipe</Text>
      </Pressable>

      <View style={styles.links}>
        <Text
          accessibilityRole="link"
          onPress={() => Linking.openURL(identity.privacyUrl)}
          style={[styles.link, { color: palette.accent }]}>
          Privacy policy
        </Text>
        <Text
          accessibilityRole="link"
          onPress={() => Linking.openURL(identity.supportUrl)}
          style={[styles.link, { color: palette.accent }]}>
          Support
        </Text>
      </View>
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { padding: Spacing.three, gap: Spacing.three },
  card: { borderRadius: 12, padding: Spacing.three, gap: Spacing.one },
  label: { fontSize: 13, fontWeight: '600' },
  value: { fontSize: 16 },
  button: {
    borderWidth: 1.5,
    borderRadius: 10,
    paddingVertical: Spacing.three - 2,
    alignItems: 'center',
  },
  buttonText: { fontSize: 16, fontWeight: '600' },
  links: { flexDirection: 'row', gap: Spacing.four, justifyContent: 'center' },
  link: { fontSize: 15 },
});
