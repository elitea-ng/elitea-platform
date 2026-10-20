import { Redirect, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import type { DiscoveryDocument } from '@/api/discovery';
import { UpgradeView } from '@/components/UpgradeRequired';
import type { BrandInput } from '@/design/brand';
import { DesignProvider } from '@/design/DesignProvider';
import { ConnectConfirm } from '@/features/connect/ConnectConfirm';
import type { ConnectCheck } from '@/features/connect/connectFlow';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { SAMPLE_HOST, SAMPLE_ORIGIN } from '@/features/designGallery/fixtures';
import { ConnectWelcome } from '@/features/entry/ConnectWelcome';
import { SignInView } from '@/features/entry/SignInView';
import { WorkspaceRow } from '@/features/workspaces/WorkspaceRow';
import { LockView } from '@/security/lock/LockGate';
import { ListSection } from '@/ui';

const SCREENS = ['connect', 'address', 'add', 'confirm', 'sign-in', 'lock', 'lock-failed', 'upgrade'] as const;
type Screen = (typeof SCREENS)[number];

/** A sample discovery document: only the fields the confirmation shows. Never sent anywhere. */
const SAMPLE_DOCUMENT = {
  display_name: 'Acme Corp AI',
  deployment_kind: 'self_hosted',
  server_version: '4.7.0',
  client_contract: '1.3',
  brand_pack_url: `${SAMPLE_ORIGIN}/api/v2/branding/pack.json`,
} as unknown as DiscoveryDocument;

/**
 * Development builds only: the signed-out screens with sample data (`/design-entry?screen=…`),
 * so connect, confirm, sign-in, lock and update can be checked and screenshot without a server
 * or a sign-out. Each renders the same view its real route renders (D2). `&brand=magenta|green|
 * lemon` applies a sample workspace accent where the real screen applies the workspace's (sign-in,
 * lock, update; never connect or confirm, which are always Zefir's).
 */
export default function DesignEntryRoute() {
  const params = useLocalSearchParams<{ screen?: string; brand?: string }>();
  if (!__DEV__) return <Redirect href="/" />;
  // The same route opened again with other params (the next screenshot) remounts the preview.
  const screen: Screen = SCREENS.includes(params.screen as Screen) ? (params.screen as Screen) : 'connect';
  const brandName = parseGalleryParams(params).brand;
  return <EntryPreview key={`${screen}-${brandName}`} screen={screen} brand={GALLERY_BRANDS[brandName]} />;
}

function EntryPreview({ screen, brand }: { screen: Screen; brand: BrandInput | null }) {
  const [address, setAddress] = useState(screen === 'address' ? SAMPLE_HOST : '');
  const workspace = {
    origin: SAMPLE_ORIGIN,
    name: brand ? 'Acme Corp AI' : null,
    logoMark: null,
    brand,
  };
  const noop = () => {};

  switch (screen) {
    case 'connect':
    case 'address':
    case 'add':
      // Connect and confirm are always Agent Zefir's own palette: no workspace brand applies yet.
      return (
        <DesignProvider brand={null}>
          <ConnectWelcome
            adding={screen === 'add'}
            backTo={screen === 'add' ? { host: 'zefir.elitea.ai', onPress: noop } : null}
            address={address}
            onAddressChange={setAddress}
            error={null}
            managedHint={false}
            busy={false}
            onContinue={noop}
            onPrivacy={noop}
            onHelp={noop}
            saved={
              screen === 'add' ? (
                <ListSection title="Your workspaces on this device">
                  <WorkspaceRow
                    workspace={{
                      id: 'aaaaaaaaaaaa',
                      origin: 'https://zefir.elitea.ai',
                      name: null,
                      logoMark: null,
                      brand: GALLERY_BRANDS.green,
                      signedIn: true,
                      requiresLock: false,
                    }}
                    active={false}
                    onPress={noop}
                  />
                </ListSection>
              ) : null
            }
          />
        </DesignProvider>
      );
    case 'confirm': {
      const check: ConnectCheck = {
        kind: 'confirm',
        origin: SAMPLE_ORIGIN,
        document: SAMPLE_DOCUMENT,
        assessment: { kind: 'ready' },
        brand: null,
      };
      return (
        <DesignProvider brand={null}>
          <ConnectConfirm check={check} devSignIn={false} busy={false} onConfirm={noop} onBack={noop} onUseCanonical={noop} />
        </DesignProvider>
      );
    }
    case 'sign-in':
      return (
        <DesignProvider brand={brand}>
          <SignInView
            host={SAMPLE_HOST}
            origin={SAMPLE_ORIGIN}
            name="Acme Corp AI"
            logo={null}
            brand={brand}
            native={{ kind: 'ready', busy: false, error: null, onSignIn: noop }}
            developer={null}
            onUseDifferentWorkspace={noop}
          />
        </DesignProvider>
      );
    case 'lock':
    case 'lock-failed':
      return (
        <DesignProvider brand={brand}>
          <LockView
            state={{
              status: 'locked',
              authInFlight: false,
              lastFailure: screen === 'lock-failed' ? 'failed' : null,
              bypassed: false,
            }}
            workspace={workspace}
            biometry="face"
            onUnlock={noop}
            onReset={noop}
            onDevBypass={null}
          />
        </DesignProvider>
      );
    case 'upgrade':
      return (
        <DesignProvider brand={brand}>
          <UpgradeView
            body="This workspace needs a newer version of Agent Zefir (0.2.0 or later). Update the app to continue."
            checking={false}
            stillRequired={false}
            others
            onGetUpdate={noop}
            onCheckAgain={noop}
            onOtherWorkspace={noop}
            onLogOut={noop}
          />
        </DesignProvider>
      );
  }
}
