// Shared integer-cents helpers.

export function assertCents(value, label = 'amount') {
  if (!Number.isSafeInteger(value)) throw new RangeError(`${label} must be an integer number of cents, got ${value}`);
  return value;
}

/**
 * percent% of a non-negative cents amount, rounded half up, computed exactly
 * for percentages with up to 3 decimals (the percent is scaled to an integer
 * number of thousandths so no binary-float rounding is involved).
 */
export function percentOfCents(amountCents, percent) {
  assertCents(amountCents);
  if (amountCents < 0) throw new RangeError('amount must not be negative');
  const milli = Math.round(percent * 1000);
  if (!Number.isFinite(percent) || milli < 0 || Math.abs(percent * 1000 - milli) > 1e-6) {
    throw new RangeError(`unsupported percentage: ${percent}`);
  }
  // amount * percent / 100 == amount * milli / 100000
  return Math.floor((amountCents * milli + 50_000) / 100_000);
}
