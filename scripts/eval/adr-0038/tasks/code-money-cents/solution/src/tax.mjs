// Sales tax. Amounts are integer cents.
import { percentOfCents } from './money.mjs';

/**
 * @param {number} amountCents taxable amount
 * @param {number} ratePercent e.g. 8.875 for 8.875 %
 */
export function taxCents(amountCents, ratePercent) {
  return percentOfCents(amountCents, ratePercent);
}
