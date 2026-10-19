const pool = require('../db');

(async () => {
  await pool.query('TRUNCATE items RESTART IDENTITY');
  await pool.query(`INSERT INTO items (name, price_cents) VALUES
    ('Notebook', 450), ('Pencil', 120), ('Backpack', 3999)`);
  console.log('seeded 3 items');
  await pool.end();
})().catch((err) => { console.error(err); process.exit(1); });
