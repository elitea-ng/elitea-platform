import { ActivityIndicator, ScrollView, StyleSheet, View } from 'react-native';

import type { ForgeCredential } from '@/api/queries/workspaceHandoff';
import { useDesign } from '@/design/DesignProvider';
import { type CredentialChoice, choiceKey, credentialLabel } from '@/features/localWork/forgeCredential';
import { Button, EmptyState, ListRow, ListSection, Text } from '@/ui';

/**
 * The `/pr-credential` sheet's content: which GitHub or GitLab configuration a cloud thread's
 * agent pushes and opens pull requests through (the web's and desktop's dialog). Choosing a row
 * saves it; None clears the choice.
 */
export function ForgeCredentialSheet({
  loading,
  failed,
  onRetry,
  choices,
  current,
  saving,
  error,
  onChoose,
}: {
  loading: boolean;
  failed: boolean;
  onRetry: () => void;
  choices: readonly CredentialChoice[];
  current: ForgeCredential | null;
  saving: boolean;
  error: string | null;
  onChoose: (credential: ForgeCredential | null) => void;
}) {
  const { colors, space } = useDesign();
  const selected = current ? choiceKey(current) : null;
  // The current choice is shown (and kept) even when the listing does not show it.
  const unlisted = current && !choices.some((choice) => choice.key === selected) ? current : null;
  return (
    <ScrollView
      style={{ backgroundColor: colors.elevated }}
      contentInsetAdjustmentBehavior="automatic"
      contentContainerStyle={[styles.content, { paddingHorizontal: space.screenGutterCompact }]}>
      <Text variant="footnote" tone="secondary">
        {
          "The cloud agent can push this thread's commits to a new branch of the folder's GitHub or GitLab repository and open a pull request (a merge request on GitLab), through the credential you choose. Nothing existing is overwritten. The credential stays on the server; you are asked before each push unless your project allows them without asking."
        }
      </Text>
      {error ? (
        <Text variant="footnote" tone="danger" accessibilityRole="alert" testID="pr-credential-error">
          {error}
        </Text>
      ) : null}
      {loading ? (
        <View style={styles.center}>
          <ActivityIndicator color={colors.accent} />
        </View>
      ) : failed ? (
        <EmptyState mark="none" title="Couldn't load credentials" body="Check your connection and try again." />
      ) : (
        <ListSection title="Credential" footer={choices.length === 0 ? 'No GitHub or GitLab credential in this project or yours. Add one on the web.' : undefined}>
          <ListRow testID="pr-credential-none" title="None" selected={selected === null} disabled={saving} onPress={() => onChoose(null)} />
          {unlisted ? <ListRow testID="pr-credential-unlisted" title={`${credentialLabel(unlisted)} (current, not listed)`} selected disabled /> : null}
          {choices.map((choice) => (
            <ListRow
              key={choice.key}
              testID={`pr-credential-${choice.key}`}
              title={credentialLabel(choice)}
              selected={selected === choice.key}
              disabled={saving}
              onPress={() => onChoose({ type: choice.type, elitea_title: choice.elitea_title, private: choice.private })}
            />
          ))}
        </ListSection>
      )}
      {failed ? <Button title="Try again" variant="tonal" onPress={onRetry} /> : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 16, gap: 16 },
  center: { padding: 24, alignItems: 'center' },
});
