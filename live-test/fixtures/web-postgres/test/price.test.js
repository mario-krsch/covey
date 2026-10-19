const test = require('node:test');
const assert = require('node:assert');
const { formatPrice, parseLegacyPrice } = require('../price');

test('formats cents as euros', () => {
  assert.strictEqual(formatPrice(1250), '12.50 €');
});

// Known red (baseline in covey.project.yml): "12" has no comma.
test('parses a legacy price without cents', () => {
  assert.strictEqual(parseLegacyPrice('12'), 1200);
});
