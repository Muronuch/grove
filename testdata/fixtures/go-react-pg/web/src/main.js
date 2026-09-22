// The API is served under the same hostname at /api, so there is no API URL to
// configure and no CORS to allow. See the [[service]] paths entry in grove.toml.
const whoami = await fetch('/api/whoami').then((r) => r.json());
document.getElementById('env').textContent = `env: ${whoami.env || 'local'}`;

const items = await fetch('/api/items').then((r) => r.json());
document.getElementById('items').innerHTML = items
  .map((i) => `<li>${i.id}: ${i.title}</li>`)
  .join('');

const ws = new WebSocket(`ws://${location.host}/ws`);
ws.onopen = () => ws.send('hello');
ws.onmessage = (e) => {
  document.getElementById('ws').textContent = e.data;
};
