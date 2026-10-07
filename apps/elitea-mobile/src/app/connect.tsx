import { useLocalSearchParams } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';
import { useEffect, useState } from 'react';

import { devSignInEnabled } from '@/auth/devSignIn';
import { identity } from '@/config/identity';
import { offerWorkspaceIconOnce } from '@/features/appIcon/appIcon';
import { ConnectConfirm } from '@/features/connect/ConnectConfirm';
import { ConnectWelcome } from '@/features/entry/ConnectWelcome';
import { checkWorkspace, confirmConnection, displayHost, type ConnectCheck } from '@/features/connect/connectFlow';
import { WorkspaceListGate } from '@/features/workspaces/WorkspaceListGate';
import { WorkspaceRow } from '@/features/workspaces/WorkspaceRow';
import { parseConnectLink, type ConnectLinkErrorCode } from '@/linking/parseConnectLink';
import { useManagedConfig } from '@/security/managedConfig';
import { MAX_WORKSPACES } from '@/security/workspaceRegistry';
import { setSession, useSession, type EndReason } from '@/state/session';
import { returnToPreviousWorkspace, switchToWorkspace } from '@/state/workspace';
import { useWorkspaceList } from '@/state/workspaceList';
import { ListSection } from '@/ui';

const ERROR_TEXT: Record<ConnectLinkErrorCode, string> = {
  empty: 'Enter your workspace address.',
  unsupported_scheme: 'That is not a workspace address or connect link.',
  not_connect_link: 'That link is not a connect link.',
  missing_server: 'The connect link does not contain a server address.',
  ambiguous_server: 'The connect link contains more than one server address.',
  nested_link: 'The connect link does not point to a web address.',
  insecure_http: 'The workspace must use a secure (https) address.',
  credentials_in_url: 'Remove the username or password from the address.',
  invalid_host: 'That server address is not valid.',
  invalid_port: 'That port number is not valid.',
  malformed: 'That address could not be read.',
};

/**
 * Why the last session ended, when the user did not end it. Every end of a
 * session removes that workspace, so this screen (not sign-in) is where it
 * lands, naming the workspace; connecting again clears `endReason`. Other
 * connected workspaces are listed below and were not affected.
 */
const END_REASON_TEXT: Partial<Record<EndReason, (host: string) => string>> = {
  device_revoked: (host) => `This device was signed out by the workspace ${host}. Its data was removed from this device.`,
  credential_rejected: (host) => `The workspace ${host} no longer accepts your sign-in. Connect and sign in again.`,
  policy: (host) => `The security policy of ${host} signed this device out.`,
};

export default function ConnectScreen() {
  // Deep links `agentzefir://connect?server=...` land here with `server` as a search param.
  const params = useLocalSearchParams<{ server?: string }>();
  const incoming = typeof params.server === 'string' ? params.server : '';
  // After a rejected credential the last origin is kept (in memory only) as a hint.
  const lastOriginHint = useSession((s) => s.lastOriginHint);
  const endReason = useSession((s) => s.endReason);
  const endedOrigin = useSession((s) => s.endedOrigin);
  const endNotice =
    endReason === null ? undefined : END_REASON_TEXT[endReason]?.(endedOrigin ? displayHost(endedOrigin) : 'you were using');
  // The other workspaces connected on this device (switching to one wipes nothing).
  const workspaces = useWorkspaceList();
  const returnTo = useSession((s) => s.returnWorkspaceId);
  const returnHost = workspaces.find((w) => w.id === returnTo);
  // MDM AppConfig `server_url` (src/security/managedConfig.ts) pre-fills an empty field.
  const managedServer = useManagedConfig().serverUrl;
  const [address, setAddress] = useState(incoming || lastOriginHint || managedServer || '');
  const [error, setError] = useState<string | null>(null);
  // Discovery result awaiting the user's confirmation; nothing is stored yet.
  const [check, setCheck] = useState<ConnectCheck | null>(null);

  // A new deep link while this screen is mounted replaces the field contents
  // (adjust-state-during-render pattern rather than an effect).
  const [lastIncoming, setLastIncoming] = useState(incoming);
  if (incoming !== lastIncoming) {
    setLastIncoming(incoming);
    if (incoming !== '') {
      setAddress(incoming);
      setError(null);
      setCheck(null);
    }
  }

  const [busy, setBusy] = useState(false);
  const devSignIn = devSignInEnabled();

  const runCheck = async (origin: string) => {
    setBusy(true);
    const result = await checkWorkspace(origin);
    setBusy(false);
    setCheck(result);
  };

  // A workspace switch (connect link while connected) lands here with the new
  // origin: check it and show the confirmation straight away.
  const pendingOrigin = useSession((s) => s.pendingConnectOrigin);
  useEffect(() => {
    if (pendingOrigin === null) return;
    let cancelled = false;
    void checkWorkspace(pendingOrigin).then((result) => {
      if (cancelled) return;
      setAddress(pendingOrigin);
      setError(null);
      setCheck(result);
      // Consumed only once shown (clearing it earlier would cancel this effect).
      setSession({ pendingConnectOrigin: null });
    });
    return () => {
      cancelled = true;
    };
  }, [pendingOrigin]);

  const onContinue = async () => {
    if (busy) return;
    const result = parseConnectLink(address, {
      connectSchemes: identity.connectSchemes,
      allowPrivateHttp: __DEV__, // dev-only: plain http to localhost / 10.x / 192.168.x
    });
    if (!result.ok) {
      setError(ERROR_TEXT[result.error]);
      return;
    }
    setError(null);
    await runCheck(result.origin);
  };

  const onConfirm = async () => {
    if (busy || check === null) return;
    setBusy(true);
    const stored = await confirmConnection(check);
    setBusy(false);
    // On success the root layout's guards route to /sign-in. A self-hosted workspace offers the
    // neutral "Zefir Night" icon, once per device (docs/design-system.md §3.6).
    if (stored && check.kind === 'confirm') void offerWorkspaceIconOnce(check.document);
    if (!stored) {
      setCheck(null);
      setError('The workspace address could not be saved on this device. Try again.');
    }
  };

  if (check !== null) {
    return (
      <ConnectConfirm
        check={check}
        devSignIn={devSignIn}
        busy={busy}
        onConfirm={() => void onConfirm()}
        onBack={() => setCheck(null)}
        onUseCanonical={(origin) => {
          setAddress(origin);
          void runCheck(origin);
        }}
      />
    );
  }

  return (
    <ConnectWelcome
      adding={workspaces.length > 0}
      backTo={returnHost ? { host: displayHost(returnHost.origin), onPress: () => void returnToPreviousWorkspace() } : null}
      endNotice={endNotice ?? null}
      address={address}
      onAddressChange={(text) => {
        setAddress(text);
        setError(null);
      }}
      error={error}
      managedHint={managedServer !== null && address.trim() === managedServer && !error}
      busy={busy}
      onContinue={() => void onContinue()}
      onPrivacy={() => void WebBrowser.openBrowserAsync(identity.privacyUrl)}
      onHelp={() => void WebBrowser.openBrowserAsync(identity.supportUrl)}
      saved={
        workspaces.length > 0 ? (
          <WorkspaceListGate workspaces={workspaces}>
            <ListSection
              title="Your workspaces on this device"
              footer={
                workspaces.length >= MAX_WORKSPACES
                  ? `At most ${MAX_WORKSPACES} workspaces can be connected. Open one and remove it from its Workspaces list to add another.`
                  : undefined
              }>
              {workspaces.map((workspace) => (
                <WorkspaceRow key={workspace.id} workspace={workspace} active={false} onPress={() => void switchToWorkspace(workspace.id)} />
              ))}
            </ListSection>
          </WorkspaceListGate>
        ) : null
      }
    />
  );
}
