// RED (R-T11-COND): a conditional with one ad-hoc operand.
// A stand-in for the app theme: the rule is syntactic, so only the shape matters.
declare const theme: {
  typography: Record<string, { fontSize: string }> & { pxToRem: (px: number) => string };
};
export const sx = (dense: boolean) => ({ fontSize: dense ? theme.typography.bodySmall.fontSize : '13px' });
