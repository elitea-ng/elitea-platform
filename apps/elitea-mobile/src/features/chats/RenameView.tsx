import { ScrollView, StyleSheet } from 'react-native';

import { CONVERSATION_NAME_MAX, cleanConversationName } from '@/api/queries/conversationActions';
import { useDesign } from '@/design/DesignProvider';
import { TextField } from '@/ui';

/** True when `draft` is a name worth saving: not blank and not the current name. */
export function canSaveName(draft: string, current: string | null | undefined): boolean {
  const name = cleanConversationName(draft);
  return name !== null && name !== (current ?? '').trim();
}

/**
 * The rename sheet's body (docs/design-system.md §4.10: a native sheet with Cancel and Save in
 * its bar): one field with the current title selected, Return saves. The route (`/rename`) and
 * its `__DEV__` preview (`/design-rename`) both render this (D2).
 */
export function RenameView({
  value,
  onChange,
  onSubmit,
  error,
}: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  error?: string | null;
}) {
  const { colors } = useDesign();
  return (
    <ScrollView
      style={{ backgroundColor: colors.elevated }}
      contentContainerStyle={styles.content}
      keyboardShouldPersistTaps="handled"
      contentInsetAdjustmentBehavior="automatic">
      <TextField
        label="Chat name"
        value={value}
        onChangeText={onChange}
        autoFocus
        selectTextOnFocus
        maxLength={CONVERSATION_NAME_MAX}
        returnKeyType="done"
        enablesReturnKeyAutomatically
        autoCapitalize="sentences"
        autoCorrect
        clearButtonMode="while-editing"
        onSubmitEditing={onSubmit}
        error={error}
        note="The new name shows on your other devices and on the web after they sync."
        testID="rename-field"
      />
    </ScrollView>
  );
}

const styles = StyleSheet.create({ content: { padding: 16, gap: 16 } });
