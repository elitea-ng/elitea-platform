import { Redirect, router, Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import { canSaveName, RenameView } from '@/features/chats/RenameView';
import { Button } from '@/ui';

/**
 * Development builds only: the rename sheet as the Chats list opens it (`/rename`, the same
 * `RenameView` and bar buttons; `sheetOptions(design, 'half')` on the route in the root stack),
 * with a sample title (`?name=`).
 */
export default function DesignRenameRoute() {
  const params = useLocalSearchParams<{ name?: string }>();
  const current = typeof params.name === 'string' && params.name !== '' ? params.name : 'Trip to Lisbon';
  const [draft, setDraft] = useState(current);
  if (!__DEV__) return <Redirect href="/" />;
  const enabled = canSaveName(draft, current);
  return (
    <>
      <Stack.Screen
        options={{
          title: 'Rename',
          headerLeft: () => <Button title="Cancel" variant="plain" size="small" onPress={() => router.back()} />,
          headerRight: () => <Button title="Save" variant="plain" size="small" disabled={!enabled} onPress={() => router.back()} />,
        }}
      />
      <RenameView value={draft} onChange={setDraft} onSubmit={() => router.back()} />
    </>
  );
}
