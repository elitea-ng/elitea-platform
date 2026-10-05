// GREEN (R-T13): the eight type-scale variants; `component` may still name a tag.
declare const theme: { typography: Record<string, Record<string, string>> };
declare function Typography(props: { variant?: string; component?: string; children?: string }): null;
declare function Counter(props: { textVariant?: string }): null;

export const spreadSx = { fontWeight: theme.typography.bodySmall.fontWeight };
export const options = { variant: 'bodyMedium' };
export const Title = () => (
  <>
    <Typography variant="headingLarge" component="h1" />
    <Counter textVariant="bodySmall" />
  </>
);
