import { router, Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import { cleanConversationName } from '@/api/queries/conversationActions';
import { canSaveName, RenameView } from '@/features/chats/RenameView';
import { useListActionsAvailable } from '@/features/chats/useChatListData';
import { useSession } from '@/state/session';
import { runConversationAction } from '@/sync/appConversationOps';
import { Button } from '@/ui';

/**
 * Rename a conversation (client contract 1.4), a native sheet over the Chats list. Save queues
 * the rename (it shows in the list at once and is sent when the workspace can take it); with no
 * offline store it is sent now and a failure stays on the sheet.
 */
export default function RenameScreen() {
  const params = useLocalSearchParams<{ id?: string; name?: string }>();
  const projectId = useSession((s) => s.projectId);
  const available = useListActionsAvailable();
  const id = Number(params.id);
  const current = typeof params.name === 'string' ? params.name : '';
  const [draft, setDraft] = useState(current);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const valid = Number.isSafeInteger(id) && id > 0 && projectId !== null && available;
  const enabled = valid && !saving && canSaveName(draft, current);

  const save = () => {
    const name = cleanConversationName(draft);
    if (!enabled || name === null || projectId === null) return;
    setSaving(true);
    setError(null);
    void runConversationAction(projectId, id, { kind: 'rename', name }).then((result) => {
      setSaving(false);
      if (result === 'failed') {
        setError("Couldn't rename the chat. Check your connection and try again.");
        return;
      }
      if (router.canGoBack()) router.back();
    });
  };

  return (
    <>
      <Stack.Screen
        options={{
          title: 'Rename',
          headerLeft: () => <Button title="Cancel" variant="plain" size="small" onPress={() => router.back()} />,
          headerRight: () => <Button title="Save" variant="plain" size="small" disabled={!enabled} onPress={save} />,
        }}
      />
      <RenameView value={draft} onChange={setDraft} onSubmit={save} error={valid ? error : 'This chat cannot be renamed here.'} />
    </>
  );
}
