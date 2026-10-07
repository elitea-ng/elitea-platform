import type { ReactNode } from 'react';
import { ScrollView, StyleSheet, View } from 'react-native';
import { KeyboardAvoidingView } from 'react-native-keyboard-controller';
import { SafeAreaView } from 'react-native-safe-area-context';

import { MaxContentWidth } from '@/constants/theme';
import { useDesign } from '@/design/DesignProvider';
import { GradientBackground } from '@/ui';

/**
 * The frame of the signed-out screens (connect, confirm, sign-in; docs/screens.md §1): the
 * pastel gradient ground, safe areas on every edge (edge-to-edge on Android, the home indicator
 * on iOS), the keyboard lifting the content (react-native-keyboard-controller, the same insets
 * as the chat), and one centred column at most `MaxContentWidth` wide that scrolls when the
 * text is large. `footer` stays at the bottom of the screen when there is room.
 */
export function EntryScreen({ children, footer, testID }: { children: ReactNode; footer?: ReactNode; testID?: string }) {
  const { space } = useDesign();
  return (
    <GradientBackground testID={testID}>
      <SafeAreaView style={styles.flex} edges={['top', 'bottom', 'left', 'right']}>
        <KeyboardAvoidingView style={styles.flex} behavior="padding">
          <ScrollView
            style={styles.flex}
            contentContainerStyle={[styles.content, { paddingHorizontal: space.screenGutter }]}
            keyboardShouldPersistTaps="handled"
            keyboardDismissMode="interactive">
            <View style={[styles.column, styles.main]}>{children}</View>
            {footer ? <View style={[styles.column, styles.footer]}>{footer}</View> : null}
          </ScrollView>
        </KeyboardAvoidingView>
      </SafeAreaView>
    </GradientBackground>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { flexGrow: 1, paddingVertical: 24, justifyContent: 'space-between', gap: 24 },
  column: { width: '100%', maxWidth: MaxContentWidth, alignSelf: 'center', gap: 16 },
  // The main column sits in the middle of the free height; the footer stays at the bottom.
  main: { flexGrow: 1, justifyContent: 'center' },
  footer: { gap: 8 },
});
