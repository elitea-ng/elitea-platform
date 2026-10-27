import { useState } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { Button, Text, TextField } from '@/ui';

import type { AgentBuilder, EditableDraft } from './useAgentBuilder';

/** The streamed text's last characters, so a person sees the model write the draft. */
const PREVIEW_CHARS = 600;

const EMPTY: EditableDraft = { name: '', description: '', instructions: '' };

/**
 * The builder's one screen: describe the agent, watch the draft stream, then
 * review it (every field editable), ask for changes, and save.
 */
export function AgentBuilderView({ builder, onSaved }: { builder: AgentBuilder; onSaved: (conversationId: number) => void }) {
  const { colors } = useDesign();
  const { state } = builder;
  const [request, setRequest] = useState('');
  const [change, setChange] = useState('');
  const [edited, setEdited] = useState<EditableDraft>(EMPTY);

  // A new draft replaces the fields under review (adjusted while rendering, not in an effect).
  const reviewed = state.phase === 'review' ? state.draft : null;
  const [shownDraft, setShownDraft] = useState<typeof reviewed>(null);
  if (reviewed !== null && reviewed !== shownDraft) {
    setShownDraft(reviewed);
    setEdited({ ...reviewed, conversation_starters: [...reviewed.conversation_starters] });
  }

  const canSave = edited.name.trim() !== '' && edited.instructions.trim() !== '' && !builder.saving;
  const save = () => {
    if (!canSave) return;
    void builder.save(edited).then((id) => {
      if (id !== null) onSaved(id);
    });
  };

  return (
    <ScrollView
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={styles.content}
      keyboardShouldPersistTaps="handled"
      contentInsetAdjustmentBehavior="automatic">
      {state.phase === 'idle' || state.phase === 'failed' ? (
        <>
          <TextField
            label="What should the agent do?"
            value={request}
            onChangeText={setRequest}
            multiline
            autoFocus
            autoCapitalize="sentences"
            error={state.phase === 'failed' ? state.failure.message : null}
            note="The agent is generated with AI. You review it before it is saved."
            testID="agent-builder-request"
          />
          <Button title="Generate" block disabled={request.trim() === ''} onPress={() => builder.generate(request)} />
        </>
      ) : null}
      {state.phase === 'streaming' ? (
        <View style={styles.streaming} accessibilityLiveRegion="polite">
          <View style={styles.row}>
            <ActivityIndicator color={colors.accent} />
            <Text variant="callout" tone="secondary">
              Writing the agent…
            </Text>
          </View>
          {state.text !== '' ? (
            <Text variant="footnote" tone="secondary" testID="agent-builder-stream">
              {state.text.length > PREVIEW_CHARS ? `…${state.text.slice(-PREVIEW_CHARS)}` : state.text}
            </Text>
          ) : null}
        </View>
      ) : null}
      {state.phase === 'review' ? (
        <>
          <TextField label="Name" value={edited.name} onChangeText={(name) => setEdited({ ...edited, name })} maxLength={32} testID="agent-builder-name" />
          <TextField label="Description" value={edited.description} onChangeText={(description) => setEdited({ ...edited, description })} multiline />
          <TextField label="Instructions" value={edited.instructions} onChangeText={(instructions) => setEdited({ ...edited, instructions })} multiline />
          <TextField
            label="Ask for a change"
            value={change}
            onChangeText={setChange}
            placeholder="For example: make it more formal"
            returnKeyType="send"
            onSubmitEditing={() => {
              builder.refine(change, edited);
              setChange('');
            }}
          />
          <Button
            title="Refine"
            variant="tonal"
            block
            disabled={change.trim() === ''}
            onPress={() => {
              builder.refine(change, edited);
              setChange('');
            }}
          />
          {builder.saveError ? (
            <Text accessibilityRole="alert" variant="callout" tone="danger">
              {builder.saveError}
            </Text>
          ) : null}
          <Button title="Save and start a chat" block loading={builder.saving} disabled={!canSave} onPress={save} testID="agent-builder-save" />
        </>
      ) : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { padding: 16, gap: 16 },
  streaming: { gap: 12 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 10 },
});
