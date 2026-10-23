import { fireEvent, render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { ConnectConfirm } from '@/features/connect/ConnectConfirm';
import { FIT_LABEL_MAX, HostText, hostSegments, segmentFit } from '@/features/connect/HostText';
import { maxScale } from '@/ui';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

describe('hostSegments (D5: the host wraps at its dots, not mid-label)', () => {
  it.each([
    ['elitea.technicaldomain.xyz', ['elitea.', 'technicaldomain.', 'xyz']],
    ['localhost:8443', ['localhost:8443']],
    ['10.0.0.5', ['10.', '0.', '0.', '5']],
    ['a..b', ['a.', '.', 'b']],
  ])('%s', (host, want) => {
    expect(hostSegments(host)).toEqual(want);
    expect(hostSegments(host).join('')).toBe(host);
  });
});

describe('HostText', () => {
  it('reads as the whole host, capped at the role scale', async () => {
    await render(<HostText heading testID="h" host="elitea.technicaldomain.xyz" role="title2" color="#000000" />);
    const host = screen.getByRole('header', { name: 'elitea.technicaldomain.xyz' });
    expect(host.props.testID).toBe('h');
    const pieces = screen.getAllByText(/./, { includeHiddenElements: true });
    for (const piece of pieces) {
      expect(piece.props.maxFontSizeMultiplier).toBe(maxScale.title2);
      // A label shrinks to stay whole on its line rather than breaking inside it.
      expect(piece.props).toMatchObject({ numberOfLines: 1, adjustsFontSizeToFit: true });
    }
  });

  it('a label too long to shrink safely wraps instead of being cut', () => {
    expect(segmentFit('a'.repeat(FIT_LABEL_MAX))).toMatchObject({ numberOfLines: 1 });
    expect(segmentFit('a'.repeat(FIT_LABEL_MAX + 1))).toEqual({});
  });
});

describe('ConnectConfirm buttons (D5: the kit pill, not radius-10 rectangles)', () => {
  const check = {
    kind: 'confirm' as const,
    origin: 'https://elitea.technicaldomain.xyz',
    document: { display_name: 'Elitea', deployment_kind: 'saas', server_version: '2.0.0' },
    assessment: { kind: 'ready' as const },
    brand: null,
  } as unknown as Parameters<typeof ConnectConfirm>[0]['check'];

  it('Connect is a pill button with horizontal padding that reads busy while connecting', async () => {
    const onConfirm = jest.fn();
    await render(
      <ConnectConfirm check={check} devSignIn={false} busy onConfirm={onConfirm} onBack={jest.fn()} onUseCanonical={jest.fn()} />,
    );
    const connect = screen.getByTestId('connect-confirm');
    const style = StyleSheet.flatten(typeof connect.props.style === 'function' ? connect.props.style({ pressed: false }) : connect.props.style);
    expect(style).toMatchObject({ borderRadius: 9999 });
    expect(style.paddingHorizontal).toBeGreaterThan(0);
    expect(connect.props.accessibilityState).toMatchObject({ busy: true, disabled: true });
    expect(screen.getByRole('header', { name: 'elitea.technicaldomain.xyz' })).toBeTruthy();
  });

  it('the back action is a kit button', async () => {
    const onBack = jest.fn();
    await render(
      <ConnectConfirm check={check} devSignIn={false} busy={false} onConfirm={jest.fn()} onBack={onBack} onUseCanonical={jest.fn()} />,
    );
    await fireEvent.press(screen.getByRole('button', { name: 'Use a different address' }));
    expect(onBack).toHaveBeenCalled();
  });
});
