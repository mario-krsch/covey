// Prices are stored in cents.
function formatPrice(cents) {
  return (cents / 100).toFixed(2) + ' €';
}

// The legacy importer's format: "12,50". Known to be wrong for whole euros,
// and listed as known red in covey.project.yml.
function parseLegacyPrice(text) {
  const [euros, cents] = text.split(',');
  return Number(euros) * 100 + Number(cents);
}

module.exports = { formatPrice, parseLegacyPrice };
