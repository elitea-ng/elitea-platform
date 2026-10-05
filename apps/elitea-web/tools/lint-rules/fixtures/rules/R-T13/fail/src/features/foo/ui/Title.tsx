// RED (R-T13): MUI stock typography variants named at a call site.
declare const theme: { typography: Record<string, Record<string, string>> };
declare function Typography(props: { variant?: string; children?: string }): null;
declare function Counter(props: { textVariant?: string }): null;

export const spreadSx = { ...theme.typography.caption };
export const options = { variant: 'body2' };
export const Title = () => (
  <>
    <Typography variant="body1" />
    <Counter textVariant="caption" />
  </>
);
