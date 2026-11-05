// Sales tax. Amounts are dollars as floating-point numbers.

/**
 * @param {number} amount taxable amount
 * @param {number} ratePercent e.g. 8.875 for 8.875 %
 */
export function taxAmount(amount, ratePercent) {
  return Math.round(amount * ratePercent) / 100;
}
