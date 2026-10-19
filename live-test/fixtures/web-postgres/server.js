// A small shop for the covey live test. One deliberate defect for the agent to
// find: an empty item name is not checked here, the database refuses it, and
// the error is not handled — the request ends in a 500 with a stack trace.
const http = require('node:http');
const pool = require('./db');
const { formatPrice } = require('./price');

const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

async function page(res) {
  const { rows } = await pool.query('SELECT id, name, price_cents FROM items ORDER BY id');
  const token = process.env.DEMO_API_TOKEN ? 'configured' : 'missing';
  res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
  res.end(`<!doctype html><title>${esc(process.env.APP_NAME || 'Shop')}</title>
<h1 id="title">${esc(process.env.APP_NAME || 'Shop')}</h1>
<p id="token">API token: ${token}</p>
<ul id="items">${rows.map((r) => `<li data-id="${r.id}">${esc(r.name)} — ${formatPrice(r.price_cents)}</li>`).join('')}</ul>
<form method="post" action="/items">
  <input id="name" name="name" placeholder="Item name">
  <input id="price" name="price" placeholder="Price in cents">
  <button id="add" type="submit">Add</button>
</form>`);
}

async function addItem(req, res) {
  let body = '';
  for await (const chunk of req) body += chunk;
  const form = new URLSearchParams(body);
  await pool.query('INSERT INTO items (name, price_cents) VALUES ($1, $2)',
    [form.get('name') || '', Number(form.get('price') || 0)]);
  res.writeHead(303, { Location: '/' });
  res.end();
}

http.createServer(async (req, res) => {
  try {
    if (req.method === 'GET' && req.url === '/health') {
      await pool.query('SELECT 1');
      res.writeHead(200);
      return res.end('ok');
    }
    if (req.method === 'GET' && req.url === '/') return await page(res);
    if (req.method === 'POST' && req.url === '/items') return await addItem(req, res);
    res.writeHead(404);
    res.end('not found');
  } catch (err) {
    res.writeHead(req.url === '/health' ? 503 : 500, { 'Content-Type': 'text/plain' });
    res.end(err.stack);
  }
}).listen(Number(process.env.PORT || 3000), '0.0.0.0', () => console.log(`listening on ${process.env.PORT || 3000}`));
