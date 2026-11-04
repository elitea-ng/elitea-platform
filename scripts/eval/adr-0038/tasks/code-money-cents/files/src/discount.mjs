// Discounts. Amounts are dollars as floating-point numbers.

/**
 * Returns the discount amount (not the discounted total).
 * @param {number} amount
 * @param {{type: 'percent', value: number} | {type: 'fixed', amount: number} | undefined} discount
 */
export function discountAmount(amount, discount) {
  if (!discount) return 0;
  if (discount.type === 'percent') return (amount * discount.value) / 100;
  if (discount.type === 'fixed') return discount.amount;
  throw new Error(`unknown discount type: ${discount.type}`);
}
