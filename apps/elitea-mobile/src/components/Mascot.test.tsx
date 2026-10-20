import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { Mascot, MASCOT_MAX_SHARP_SIZE, MASCOT_NAME, mascotAspect } from '@/components/Mascot';
import { DesignProvider } from '@/design/DesignProvider';
import { colors } from '@/design/tokens';

// The app's tsconfig has no Node types; jest runs this file in Node.
declare const __dirname: string;
const fs = require('fs') as { readFileSync(path: string): Uint8Array };

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

/** Pixel size from the PNG's IHDR chunk, read from the file the component bundles. */
function pngSize(file: string): { width: number; height: number } {
  const buf = fs.readFileSync(`${__dirname}/../../assets/images/mascot/${file}`);
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  expect(String.fromCharCode(...buf.subarray(12, 16))).toBe('IHDR');
  return { width: view.getUint32(16), height: view.getUint32(20) };
}

const FILES = { avatar: 'avatar-head', bust: 'mascot-bust', full: 'mascot-full', hero: 'mascot-hero' } as const;

describe('mascot artwork', () => {
  it.each(Object.entries(FILES))('%s: the aspect constant matches %s.png', (variant, file) => {
    const { width, height } = pngSize(`${file}.png`);
    expect(mascotAspect(variant as keyof typeof FILES)).toBeCloseTo(width / height, 3);
  });

  it.each(Object.entries(FILES))('%s: the sharp-size limit is the widest file / 3', (variant, file) => {
    const widest = Math.max(
      ...['', '@2x', '@3x'].map((sfx) => {
        try {
          return pngSize(`${file}${sfx}.png`).width;
        } catch {
          return 0;
        }
      }),
    );
    expect(MASCOT_MAX_SHARP_SIZE[variant as keyof typeof FILES]).toBe(Math.floor(widest / 3));
  });
});

describe('<Mascot>', () => {
  it('is an image labelled "Agent Zefirca" by default', async () => {
    await render(<Mascot />);
    expect(MASCOT_NAME).toBe('Agent Zefirca');
    expect(screen.getByRole('image', { name: 'Agent Zefirca' })).toBeOnTheScreen();
  });

  it('takes the agent name when it stands in for an agent avatar', async () => {
    await render(<Mascot variant="avatar" size={28} label="Research agent" />);
    expect(screen.getByRole('image', { name: 'Research agent' })).toBeOnTheScreen();
  });

  it('decorative: hidden from assistive technology', async () => {
    await render(<Mascot variant="full" decorative testID="m" />);
    expect(screen.queryByRole('image')).toBeNull();
    expect(screen.getByTestId('m', { includeHiddenElements: true }).props.accessible).toBe(false);
  });

  it.each(['avatar', 'bust', 'full', 'hero'] as const)('%s keeps its aspect ratio', async (variant) => {
    await render(<Mascot variant={variant} size={120} testID="m" />);
    const style = StyleSheet.flatten(screen.getByTestId('m').props.style);
    expect(style.width).toBe(120);
    expect(style.height).toBe(variant === 'avatar' ? 120 : Math.round(120 / mascotAspect(variant)));
  });

  it('never renders below the 24 pt minimum', async () => {
    await render(<Mascot variant="avatar" size={12} testID="m" />);
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).width).toBe(24);
  });

  it.each(['light', 'dark'] as const)('the avatar disc is the fixed Zefir soft rose in %s', async (scheme) => {
    await render(
      <DesignProvider scheme={scheme}>
        <Mascot variant="avatar" size={40} testID="m" />
      </DesignProvider>,
    );
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).backgroundColor).toBe(colors[scheme].accentSoft);
  });
});
