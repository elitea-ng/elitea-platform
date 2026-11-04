import test from 'node:test';
import assert from 'node:assert/strict';
import { createCart, addItem, lineTotalCents, subtotalCents } from '../src/cart.mjs';
import { discountCents } from '../src/discount.mjs';
import { taxCents } from '../src/tax.mjs';
import { formatCents } from '../src/format.mjs';
import { summarize } from '../src/checkout.mjs';

test('cart stores integer cents and merges by sku', () => {
  const cart = createCart();
  addItem(cart, { sku: 'w', name: 'Widget', priceCents: 1999, qty: 2 });
  addItem(cart, { sku: 'w', name: 'Widget', priceCents: 1999 });
  addItem(cart, { sku: 'g', name: 'Gadget', priceCents: 10 });
  assert.equal(cart.items.length, 2);
  assert.equal(lineTotalCents(cart.items[0]), 5997);
  assert.equal(subtotalCents(cart), 6007);
});

test('cart rejects non-integer or negative cents and bad quantities', () => {
  const cart = createCart();
  assert.throws(() => addItem(cart, { sku: 'a', name: 'A', priceCents: 19.99 }), RangeError);
  assert.throws(() => addItem(cart, { sku: 'a', name: 'A', priceCents: -1 }), RangeError);
  assert.throws(() => addItem(cart, { sku: 'a', name: 'A', priceCents: 100, qty: 0 }), RangeError);
  assert.throws(() => addItem(cart, { sku: 'a', name: 'A', priceCents: 100, qty: 1.5 }), RangeError);
  assert.equal(cart.items.length, 0);
});

test('percent discount rounds half up to the cent', () => {
  assert.equal(discountCents(1005, { type: 'percent', value: 10 }), 101);
  assert.equal(discountCents(999, { type: 'percent', value: 10 }), 100);
  assert.equal(discountCents(1500, { type: 'percent', value: 33.3 }), 500, '1500 * 33.3% = 499.5 -> 500');
});

test('fixed discount is in cents and capped at the amount', () => {
  assert.equal(discountCents(5000, { type: 'fixed', amountCents: 750 }), 750);
  assert.equal(discountCents(500, { type: 'fixed', amountCents: 800 }), 500);
  assert.equal(discountCents(500, undefined), 0);
  assert.throws(() => discountCents(500, { type: 'bogus' }), Error);
});

test('tax rounds half up on exact decimal rates', () => {
  assert.equal(taxCents(1500, 2.3), 35, '1500 * 2.3% = 34.5 -> 35 (float math gives 34.4999...)');
  assert.equal(taxCents(1000, 8.875), 89);
  assert.equal(taxCents(1234, 0), 0);
  assert.equal(taxCents(0, 7.25), 0);
});

test('formatCents renders dollars with thousands separators', () => {
  assert.equal(formatCents(0), '$0.00');
  assert.equal(formatCents(5), '$0.05');
  assert.equal(formatCents(99), '$0.99');
  assert.equal(formatCents(123450), '$1,234.50');
  assert.equal(formatCents(100000000), '$1,000,000.00');
});

test('formatCents handles negatives and rejects fractional cents', () => {
  assert.equal(formatCents(-5), '-$0.05');
  assert.equal(formatCents(-123456), '-$1,234.56');
  assert.throws(() => formatCents(19.99), RangeError);
});

test('summarize: discount before tax, everything in cents', () => {
  const cart = createCart();
  addItem(cart, { sku: 'w', name: 'Widget', priceCents: 1999, qty: 3 });
  addItem(cart, { sku: 'g', name: 'Gadget', priceCents: 2501 });
  const s = summarize(cart, { discount: { type: 'percent', value: 15 }, taxRatePercent: 8.875 });
  assert.deepEqual(s, {
    subtotalCents: 8498,
    discountCents: 1275,
    taxCents: 641,
    totalCents: 7864,
    display: { subtotal: '$84.98', discount: '$12.75', tax: '$6.41', total: '$78.64' },
  });
});

test('summarize with no discount and no tax', () => {
  const cart = createCart();
  addItem(cart, { sku: 'a', name: 'A', priceCents: 10, qty: 3 });
  addItem(cart, { sku: 'b', name: 'B', priceCents: 20 });
  const s = summarize(cart);
  assert.equal(s.subtotalCents, 50);
  assert.equal(s.totalCents, 50);
  assert.equal(s.display.total, '$0.50');
});

test('summarize: fixed discount larger than subtotal yields zero total', () => {
  const cart = createCart();
  addItem(cart, { sku: 'a', name: 'A', priceCents: 300 });
  const s = summarize(cart, { discount: { type: 'fixed', amountCents: 1000 }, taxRatePercent: 10 });
  assert.equal(s.discountCents, 300);
  assert.equal(s.taxCents, 0);
  assert.equal(s.totalCents, 0);
});

test('summarize always returns integer cents', () => {
  const prices = [1, 33, 101, 999, 1015, 2999, 12345];
  for (const rate of [0, 2.3, 6.25, 8.875, 19.6]) {
    for (const pct of [0, 5, 12.5, 33.3]) {
      const cart = createCart();
      prices.forEach((p, i) => addItem(cart, { sku: `s${i}`, name: `S${i}`, priceCents: p, qty: (i % 3) + 1 }));
      const s = summarize(cart, { discount: { type: 'percent', value: pct }, taxRatePercent: rate });
      for (const k of ['subtotalCents', 'discountCents', 'taxCents', 'totalCents']) {
        assert.ok(Number.isInteger(s[k]), `${k} = ${s[k]} for rate ${rate}, pct ${pct}`);
      }
      assert.equal(s.totalCents, s.subtotalCents - s.discountCents + s.taxCents);
    }
  }
});
