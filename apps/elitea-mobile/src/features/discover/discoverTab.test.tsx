import { act, render, screen } from '@testing-library/react-native';
import type { ReactElement, ReactNode } from 'react';

import { discoveryDocument, discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import TabsLayout from '@/app/(app)/(tabs)/_layout';
import { FAKE_ORIGIN } from '@/test/fakeServer';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';

// The app's tsconfig has no Node types; jest runs this file in Node.
const fs = require('fs') as { readFileSync(path: string, encoding: 'utf8'): string };
declare const require: { (id: string): unknown; resolve(id: string): string };

/** Every NativeTabsView mount: one per tab-bar configuration the stand-in rendered. */
const mounts: string[] = [];

// The stand-in keeps expo-router's own rule (NativeBottomTabsNavigator.js): hidden
// triggers are dropped, and the tab view is keyed by the remaining trigger names, so
// a change of that set remounts the bar and every tab's content. The last test below
// checks that the installed expo-router still does this.
jest.mock('expo-router/native-tabs', () => {
  const { Text: T, View: V } = jest.requireActual<typeof import('react-native')>('react-native');
  const React = jest.requireActual<typeof import('react')>('react');
  const Trigger = ({ name, children }: { name: string; hidden?: boolean; children?: ReactNode }) => (
    <V testID={`tab-${name}`}>{children}</V>
  );
  function Label({ children }: { children: ReactNode }) {
    return <T>{children}</T>;
  }
  function Icon() {
    return null;
  }
  Trigger.Label = Label;
  Trigger.Icon = Icon;
  function TabsView({ configurationKey, children }: { configurationKey: string; children: ReactNode }) {
    React.useEffect(() => {
      mounts.push(configurationKey);
    }, []); // eslint-disable-line react-hooks/exhaustive-deps -- mount only
    return <V>{children}</V>;
  }
  const NativeTabs = ({ children }: { children: ReactNode }) => {
    const visible = React.Children.toArray(children).filter(
      (child): child is ReactElement<{ name: string; hidden?: boolean }> =>
        React.isValidElement(child) && child.type === Trigger && !(child.props as { hidden?: boolean }).hidden,
    );
    const configurationKey = visible.map((child) => child.props.name).join(';');
    return (
      <TabsView key={configurationKey} configurationKey={configurationKey}>
        {visible}
      </TabsView>
    );
  };
  NativeTabs.Trigger = Trigger;
  return { NativeTabs };
});

beforeEach(() => {
  __resetWorkspaceInfoForTests();
  mounts.length = 0;
});

function workspace(doc: Record<string, unknown>) {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(doc) });
}

describe('the tab bar', () => {
  it.each([
    ['1.3', discoveryDocument13()],
    ['1.1', discoveryDocument11()],
    ['1.0', discoveryDocument()],
  ])('on a %s workspace shows Home, Chats, Discover and Settings (the Discover screen refuses below 1.3)', async (_label, doc) => {
    workspace(doc);
    await render(<TabsLayout />);
    expect(screen.getByTestId('tab-(home)')).toBeTruthy();
    expect(screen.getByTestId('tab-chats')).toBeTruthy();
    expect(screen.getByTestId('tab-discover')).toBeTruthy();
    expect(screen.getByTestId('tab-settings')).toBeTruthy();
    expect(screen.getByText('Discover')).toBeTruthy();
  });

  it('keeps one configuration, so nothing remounts, when the workspace becomes known or changes contract', async () => {
    await render(<TabsLayout />);
    await act(async () => workspace(discoveryDocument13()));
    await act(async () => workspace(discoveryDocument11()));
    await act(async () => workspace(discoveryDocument13()));
    expect(mounts).toEqual(['(home);chats;discover;settings']);
  });

  it("matches the installed expo-router: hidden triggers leave the set and the set keys the tab view", () => {
    const source = fs.readFileSync(require.resolve('expo-router/build/native-tabs/NativeBottomTabsNavigator.js'), 'utf8');
    expect(source).toMatch(/filter\(\(child\)=>!child\.props\.hidden\)/);
    expect(source).toMatch(/tabConfigurationKey = triggerChildren\.map\(\(child\)=>child\.props\.name\)\.join\(';'\)/);
    expect(source).toMatch(/key: tabConfigurationKey/);
  });
});
