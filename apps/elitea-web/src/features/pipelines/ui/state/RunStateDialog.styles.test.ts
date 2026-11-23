import { describe, expect, it } from 'vitest';

import type { SxProps, Theme } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import {
  dialogPaperSx,
  runStatusContainerSx,
  runStatusTextSx,
} from './RunStateDialog.styles';

const theme: Theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function resolve(sxValue: SxProps<Theme>): Record<string, unknown> {
  return (sxValue as (resolvedTheme: Theme) => Record<string, unknown>)(theme);
}

describe('RunStateDialog.styles', () => {
  it('runStatusContainerSx/runStatusTextSx pick a border/text colour per status', () => {
    const completed = resolve(runStatusContainerSx('Completed'));
    const errored = resolve(runStatusContainerSx('Error'));
    expect(completed['border']).not.toBe(errored['border']);

    const completedText = resolve(runStatusTextSx('Completed'));
    const errorText = resolve(runStatusTextSx('Error'));
    expect(completedText['color']).not.toBe(errorText['color']);
  });

  it('dialogPaperSx scales width/height off the editor dimensions', () => {
    const result = resolve(dialogPaperSx(1000, 800));
    expect(result['width']).toBe('900px');
    expect(result['maxHeight']).toBe('640px');
  });
});
