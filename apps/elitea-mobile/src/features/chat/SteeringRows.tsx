import { useState } from 'react';
import { StyleSheet, TextInput, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { Button } from '@/ui';
import { STEERING_STATE, isSteeringTextValid, visibleSteering, type SteeringItem } from '@elitea/client-core/stream';

/**
 * The steering messages of the live answer (client contract 1.26): the
 * messages sent while the answer runs. Pending ones say "Waiting for the next
 * step"; the author can edit or remove them, or stop the answer and send
 * them at once ("Stop and send": the end-of-turn rule then sends them as the
 * next message). Delivered ones say "Delivered".
 */

/**
 * The live turn's steering messages to draw, in queue order. Removed and
 * `turn_ended` ones are not drawn, and neither is a delivered one whose
 * `message_id` is already a row of the transcript (after the refetch the
 * server sends it as an ordinary user message: no double).
 */
export function steeringRows(items: readonly SteeringItem[], transcriptIds: ReadonlySet<string>): SteeringItem[] {
  return visibleSteering(items, transcriptIds).filter(
    (item) => item.state === STEERING_STATE.Pending || item.state === STEERING_STATE.Injected,
  );
}

export function SteeringMessage({
  item,
  own,
  onEdit,
  onRemove,
  onStopAndSend,
}: {
  item: SteeringItem;
  /** This device queued it: it can be edited or removed. */
  own: boolean;
  /** Resolves to an error text, or null. */
  onEdit: (id: string, text: string) => Promise<string | null>;
  onRemove: (id: string) => Promise<string | null>;
  /** Stops the answer; null when it cannot be stopped from here. */
  onStopAndSend: (() => void) | null;
}) {
  const { colors } = useDesign();
  const [draft, setDraft] = useState<string | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const pending = item.state === STEERING_STATE.Pending;

  const settle = (result: Promise<string | null>, closeEdit: boolean) => {
    setSaving(true);
    void result
      .then((error) => {
        setProblem(error);
        if (closeEdit) setDraft(null);
      })
      .finally(() => setSaving(false));
  };

  if (draft !== null) {
    return (
      <MessageBubble
        role="user"
        error={problem}
        actions={
          <View style={styles.actions}>
            <Button
              title="Save"
              variant="tonal"
              size="small"
              disabled={saving || !isSteeringTextValid(draft)}
              onPress={() => settle(onEdit(item.id, draft), true)}
            />
            <Button title="Cancel" variant="tonal" size="small" onPress={() => setDraft(null)} />
          </View>
        }>
        <TextInput
          value={draft}
          onChangeText={setDraft}
          multiline
          autoFocus
          accessibilityLabel="Edit the waiting message"
          style={[styles.input, { color: colors.textPrimary, borderColor: colors.separator }]}
        />
      </MessageBubble>
    );
  }

  const actions =
    pending && own ? (
      <View style={styles.actions}>
        <Button title="Edit" variant="tonal" size="small" disabled={saving} onPress={() => setDraft(item.content)} />
        <Button title="Remove" variant="tonal" size="small" disabled={saving} onPress={() => settle(onRemove(item.id), false)} />
        {onStopAndSend ? <Button title="Stop and send" variant="tonal" size="small" disabled={saving} onPress={onStopAndSend} /> : null}
      </View>
    ) : undefined;
  return (
    <MessageBubble role="user" mine={own} status={pending ? 'Waiting for the next step' : 'Delivered'} error={problem} actions={actions}>
      {item.content}
    </MessageBubble>
  );
}

const styles = StyleSheet.create({
  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  input: { minHeight: 44, borderWidth: StyleSheet.hairlineWidth, borderRadius: 8, paddingHorizontal: 8, paddingVertical: 6 },
});
