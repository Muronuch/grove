'use strict';
const http = require('node:http');
const { connect, pool } = require('./db');

let db;

const routes = {
  '/healthz': async () => {
    await db.query('SELECT 1');
    return { body: 'ok\n', type: 'text/plain' };
  },
  '/api/whoami': async () => ({
    json: {
      env: process.env.GROVE_ENV || '',
      slot: process.env.GROVE_SLOT || '',
      project: process.env.GROVE_PROJECT || '',
    },
  }),
  '/api/items': async () => {
    const [rows] = await db.query('SELECT id, title FROM items ORDER BY id');
    return { json: rows };
  },
};

async function main() {
  // Wait for the database once, then serve through a pool.
  await (await connect()).end();
  db = pool();

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost');
    const handler = routes[url.pathname];
    if (!handler) {
      res.writeHead(404, { 'content-type': 'text/plain' });
      res.end('not found\n');
      return;
    }
    try {
      const out = await handler(req);
      if (out.json !== undefined) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify(out.json));
      } else {
        res.writeHead(200, { 'content-type': out.type || 'text/plain' });
        res.end(out.body);
      }
    } catch (err) {
      res.writeHead(500, { 'content-type': 'text/plain' });
      res.end(`${err.message}\n`);
    }
  });

  server.listen(3000, '0.0.0.0', () => {
    console.log(`api listening on :3000 (env=${process.env.GROVE_ENV || 'local'})`);
  });
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
