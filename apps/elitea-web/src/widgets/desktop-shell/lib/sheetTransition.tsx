/** A sheet drops from the toolbar edge and rises back: MUI's Slide, from the top. */
import { forwardRef, type ReactElement, type Ref } from 'react';

import Slide, { type SlideProps } from '@mui/material/Slide';

export const SheetTransition = forwardRef(function SheetTransition(props: SlideProps & { children: ReactElement }, ref: Ref<unknown>) {
  return <Slide direction="down" ref={ref} {...props} />;
});
