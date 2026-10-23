import { router } from 'expo-router';
import { useEffect } from 'react';
import { Alert } from 'react-native';

import { identity } from '@/config/identity';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspacesSheet } from '@/features/workspaces/WorkspacesSheet';
import { log } from '@/lib/log';
import { useSession } from '@/state/session';
import { beginAddWorkspace, removeWorkspace, switchToWorkspace } from '@/state/workspace';
import { refreshWorkspaceList, useWorkspaceList, type WorkspaceSummary } from '@/state/workspaceList';

/**
 * The workspace switcher (a sheet with detents, `sheetOptions`): every workspace connected on
 * this device, each with its host and in its own accent. Switching never deletes anything;
 * "Remove" logs out of that one workspace and deletes only its data; "Add workspace" opens the
 * connect flow with the current workspace kept.
 */
export default function WorkspacesScreen() {
  const activeId = useSession((s) => s.workspaceId);
  const workspaces = useWorkspaceList();

  useEffect(() => {
    void refreshWorkspaceList();
  }, []);

  const onOpen = (workspace: WorkspaceSummary) => {
    if (router.canGoBack()) router.back();
    if (workspace.id === activeId) return;
    void switchToWorkspace(workspace.id).then((ok) => {
      if (!ok) log.warn('workspace could not be opened');
    });
  };

  const onRemove = (workspace: WorkspaceSummary): Promise<boolean> =>
    new Promise((resolve) => {
      const host = displayHost(workspace.origin);
      const active = workspace.id === activeId;
      Alert.alert(
        `Remove ${host}?`,
        `This signs you out of ${host} and deletes everything ${identity.displayName} stored for it on this device.${
          workspaces.length > 1 ? ' Your other workspaces are not affected.' : ''
        }`,
        [
          { text: 'Cancel', style: 'cancel', onPress: () => resolve(false) },
          {
            text: 'Log out & remove',
            style: 'destructive',
            onPress: () => {
              if (active && router.canGoBack()) router.back();
              void removeWorkspace(workspace.id).then(resolve, () => resolve(false));
            },
          },
        ],
        { cancelable: true, onDismiss: () => resolve(false) },
      );
    });

  return (
    <WorkspacesSheet
      workspaces={workspaces}
      activeId={activeId}
      onOpen={onOpen}
      onRemove={onRemove}
      onAdd={() => {
        if (router.canGoBack()) router.back();
        void beginAddWorkspace();
      }}
    />
  );
}
