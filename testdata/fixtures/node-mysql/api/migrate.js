'use strict';
// Applies every unapplied file in migrations/ and then seeds. Running it twice
// is a no-op, which is what lets grove clone a prepared database and apply only
// a branch's own new migrations on top of it.
const fs = require('node:fs');
const path = require('node:path');
const { connect } = require('./db');

function sqlFiles(dir) {
  const full = path.join(__dirname, dir);
  if (!fs.existsSync(full)) return [];
  return fs
    .readdirSync(full)
    .filter((f) => f.endsWith('.sql'))
    .sort()
    .map((f) => ({ name: path.basename(f, '.sql'), path: path.join(full, f) }));
}

async function main() {
  const db = await connect();
  await db.query(`CREATE TABLE IF NOT EXISTS schema_migrations (
    version VARCHAR(255) PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
  )`);

  for (const m of sqlFiles('migrations')) {
    const [rows] = await db.query('SELECT 1 FROM schema_migrations WHERE version = ?', [m.name]);
    if (rows.length > 0) {
      console.log(`migrate: ${m.name} already applied`);
      continue;
    }
    await db.query(fs.readFileSync(m.path, 'utf8'));
    await db.query('INSERT INTO schema_migrations (version) VALUES (?)', [m.name]);
    console.log(`migrate: applied ${m.name}`);
  }

  for (const s of sqlFiles('seed')) {
    await db.query(fs.readFileSync(s.path, 'utf8'));
    console.log(`migrate: seeded ${s.name}`);
  }

  console.log('migrate: up to date');
  await db.end();
}

main().catch((err) => {
  console.error(`migrate failed: ${err.message}`);
  process.exit(1);
});
