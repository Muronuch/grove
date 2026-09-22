// Every env would send its requests to the same backend.
const API = "http://localhost:8080/api";
export async function items() {
  return fetch(`${API}/items`).then((r) => r.json());
}
