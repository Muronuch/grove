'use strict';
const mysql = require('mysql2/promise');

const config = {
  host: process.env.DATABASE_HOST || 'db',
  user: process.env.DATABASE_USER || 'app',
  password: process.env.DATABASE_PASSWORD || 'app',
  database: process.env.DATABASE_NAME || 'app',
  multipleStatements: true,
};

// connect retries: the database is healthy before it accepts application
// connections, and a migration run must not fail on that gap.
async function connect(timeoutMs = 90_000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  for (;;) {
    try {
      return await mysql.createConnection(config);
    } catch (err) {
      last = err;
      if (Date.now() > deadline) throw last;
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
}

// pool is what the server uses: a single long-lived connection goes stale when
// the database restarts or times it out, which a snapshot restore does.
function pool() {
  return mysql.createPool({ ...config, waitForConnections: true, connectionLimit: 5 });
}

module.exports = { connect, pool };
