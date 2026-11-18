import { Stack, useLocalSearchParams } from 'expo-router';
import { useMemo, useState } from 'react';
import { Alert, RefreshControl } from 'react-native';

import { useAgents } from '@/api/queries/agents';
import {
  DELEGATION_COPY,
  DelegationError,
  approveDelegationGrant,
  delegationErrorText,
  revokeDelegationGrant,
  useDelegationGrants,
  useDelegationRefresh,
  type DelegationGrantView,
} from '@/api/queries/delegation';
import { useDesign } from '@/design/DesignProvider';
import { calleeName, DelegationGrantsView } from '@/features/delegation/DelegationGrantsView';
import { useDelegationAvailable } from '@/features/delegation/useDelegationAvailable';
import { useShareOut } from '@/security/policy';
import { getSession, useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** The agent's name as passed in the route, cut to something a title can hold. */
function nameParam(value: unknown): string {
  const name = typeof value === 'string' ? value.trim().slice(0, 120) : '';
  return name === '' ? 'This agent' : name;
}

/**
 * The delegation grants of one agent (ADR-0044 amendment 1, client contract 1.59): what each
 * agent attached to it was approved to use. The app has no agent editor, so this is the grant
 * list with Approve and Revoke; attaching, and a governed grant's change request, are on the web
 * and desktop. Online only: nothing here is kept on the device.
 */
export default function AgentDelegationsScreen() {
  const { colors } = useDesign();
  const { id, name } = useLocalSearchParams<{ id: string; name?: string }>();
  const applicationId = typeof id === 'string' && /^[1-9]\d{0,14}$/.test(id) ? Number(id) : null;
  const agentName = nameParam(name);
  // The agent id means something only in the project it was opened from: pin it at mount.
  const [opened] = useState(() => ({ workspaceId: getSession().workspaceId, projectId: getSession().projectId }));
  const projectId = opened.projectId;
  const moved = useSession((s) => s.workspaceId !== opened.workspaceId || s.projectId !== opened.projectId);
  const workspace = useWorkspaceInfo();
  const supported = useDelegationAvailable();
  const grants = useDelegationGrants(projectId, applicationId, supported && !moved);
  const refresh = useDelegationRefresh(projectId, applicationId);
  const agents = useAgents(supported && !moved ? projectId : null);
  const selectable = useShareOut();
  const [note, setNote] = useState<string | null>(null);
  const [busy, setBusy] = useState<number | null>(null);

  const names = useMemo(() => Object.fromEntries((agents.data ?? []).map((agent) => [agent.id, agent.name])), [agents.data]);
  const header = <Stack.Screen options={{ title: agentName === 'This agent' ? 'Delegations' : `${agentName}'s delegations` }} />;

  const stillHere = () => {
    const now = getSession();
    return now.workspaceId === opened.workspaceId && now.projectId === opened.projectId;
  };

  const run = (grant: DelegationGrantView, action: () => Promise<unknown>, fallback: string) => {
    setBusy(grant.id);
    action()
      .then(() => setNote(null))
      .catch((error: unknown) => setNote(delegationErrorText(error, fallback)))
      // A refusal can mean the grant changed: read the list again either way.
      .finally(() => {
        setBusy(null);
        void refresh();
      });
  };

  const onApprove = (grant: DelegationGrantView) => {
    if (projectId === null || applicationId === null || busy !== null) return;
    const callee = calleeName(grant, names);
    Alert.alert(
      `Approve ${callee}?`,
      `${agentName} will be able to use what ${callee} brings, as listed, for the parts you hold access to. What you do not hold access to stays out.`,
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Approve', onPress: () => stillHere() && run(grant, () => approveDelegationGrant(projectId, applicationId, grant.id), DELEGATION_COPY.approve) },
      ],
    );
  };

  const onRevoke = (grant: DelegationGrantView) => {
    if (projectId === null || applicationId === null || busy !== null) return;
    const callee = calleeName(grant, names);
    Alert.alert(`Revoke ${callee}?`, `${agentName} can no longer use what ${callee} brings that it does not hold itself. Attach it again to ask for approval.`, [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Revoke', style: 'destructive', onPress: () => stillHere() && run(grant, () => revokeDelegationGrant(projectId, applicationId, grant.id), DELEGATION_COPY.revoke) },
    ]);
  };

  const refetch = () => {
    setNote(null);
    void grants.refetch();
  };

  if (applicationId === null || projectId === null || !supported || moved) {
    // No discovery document yet (an offline launch) means "unknown", not "not offered".
    const message =
      applicationId === null
        ? 'This agent could not be found.'
        : projectId === null
          ? 'Choose a project first.'
          : moved
            ? 'You switched project or workspace. Go back and open this agent again.'
            : workspace === null
              ? DELEGATION_COPY.offline
              : DELEGATION_COPY.notAvailable;
    return (
      <>
        {header}
        <DelegationGrantsView
          agentName={agentName}
          status={{ kind: 'unavailable', message }}
          grants={[]}
          names={{}}
          busy={null}
          banner={null}
          selectable={false}
          onApprove={() => {}}
          onRevoke={() => {}}
          onRetry={() => {}}
        />
      </>
    );
  }

  const failed = grants.error;
  const rows = grants.data ?? [];
  return (
    <>
      {header}
      <DelegationGrantsView
        agentName={agentName}
        status={
          grants.isLoading
            ? { kind: 'loading' }
            : failed
              ? failed instanceof DelegationError && failed.reason === 'not_available'
                ? { kind: 'unavailable', message: failed.message }
                : { kind: 'error', message: delegationErrorText(failed) }
              : { kind: 'ready' }
        }
        grants={rows}
        names={names}
        busy={busy}
        banner={note ?? (failed && rows.length > 0 ? delegationErrorText(failed) : null)}
        selectable={selectable}
        onApprove={onApprove}
        onRevoke={onRevoke}
        onRetry={refetch}
        refreshControl={<RefreshControl refreshing={grants.isRefetching} onRefresh={refetch} tintColor={colors.accent} />}
      />
    </>
  );
}
