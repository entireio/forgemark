// Thin fetch wrappers over the forgemark API. All errors surface as thrown
// Error objects carrying the server's message.

async function req(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* non-JSON error body */ }
  if (!res.ok) {
    throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  }
  return data;
}

export const api = {
  startRun: (spec) => req('POST', '/api/runs', spec),
  listRuns: () => req('GET', '/api/runs'),
  getRun: (id) => req('GET', `/api/runs/${encodeURIComponent(id)}`),
  cancelRun: (id) => req('POST', `/api/runs/${encodeURIComponent(id)}/cancel`),
  history: () => req('GET', '/api/history'),
  historyDoc: (file) => req('GET', `/api/history/${encodeURIComponent(file)}`),
  localSuggest: () => req('GET', '/api/local/suggest'),
};
