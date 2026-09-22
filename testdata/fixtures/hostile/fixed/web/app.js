// Read from the environment, so each env talks to its own backend.
const API = import.meta.env.API_URL;
export async function items() {
  return fetch(`${API}/items`).then((r) => r.json());
}
