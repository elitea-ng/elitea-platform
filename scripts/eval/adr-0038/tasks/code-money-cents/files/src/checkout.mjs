// Checkout summary built from the cart, discount, tax and format modules.
import { subtotal } from './cart.mjs';
import { discountAmount } from './discount.mjs';
import { taxAmount } from './tax.mjs';
import { formatMoney } from './format.mjs';

/**
 * @param {{items: object[]}} cart
 * @param {{discount?: object, taxRatePercent?: number}} [options]
 */
export function summarize(cart, { discount, taxRatePercent = 0 } = {}) {
  const sub = subtotal(cart);
  const disc = discountAmount(sub, discount);
  const taxable = sub - disc;
  const tax = taxAmount(taxable, taxRatePercent);
  const total = taxable + tax;
  return {
    subtotal: sub,
    discount: disc,
    tax,
    total,
    display: {
      subtotal: formatMoney(sub),
      discount: formatMoney(disc),
      tax: formatMoney(tax),
      total: formatMoney(total),
    },
  };
}
