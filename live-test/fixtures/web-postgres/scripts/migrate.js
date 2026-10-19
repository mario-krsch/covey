const pool = require('../db');

(async () => {
  await pool.query(`CREATE TABLE IF NOT EXISTS items (
    id serial PRIMARY KEY,
    name text NOT NULL CHECK (name <> ''),
    price_cents integer NOT NULL DEFAULT 0
  )`);
  console.log('migrated');
  await pool.end();
})().catch((err) => { console.error(err); process.exit(1); });
