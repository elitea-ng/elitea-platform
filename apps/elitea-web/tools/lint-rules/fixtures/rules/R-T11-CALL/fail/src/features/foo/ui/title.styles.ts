// RED (R-T11-CALL): a computed size (pxToRem / calc helpers).
// A stand-in for the app theme: the rule is syntactic, so only the shape matters.
declare const theme: {
  typography: Record<string, { fontSize: string }> & { pxToRem: (px: number) => string };
};
export const sx = { fontSize: theme.typography.pxToRem(13) };
