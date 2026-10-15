export class ApiError extends Error {
  constructor(code, message) { super(message); this.code = code; }
}

// Deliberately has no configurable host. Redirects cannot carry credentials away.
export function client(key, name, signal) {
  return async (path, body) => {
    if (!path.startsWith('/v1/') || path.includes('://')) throw new Error('Invalid API path');
    const res = await fetch(new URL(path, location.origin), {
      method: body === undefined ? 'GET' : 'POST',
      headers: { ...(key ? { Authorization: `Bearer ${key}`, 'X-Wayglass-Agent': name } : {}),
        Accept: 'application/json', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      body: body === undefined ? undefined : JSON.stringify(body),
      mode: 'same-origin', credentials: 'omit', redirect: 'error', cache: 'no-store',
      signal: AbortSignal.any([signal, AbortSignal.timeout(15000)]),
    });
    let envelope;
    try { envelope = await res.json(); }
    catch { throw new ApiError('bad_response', `Unexpected Worker response (${res.status}).`); }
    if (!res.ok || !envelope.ok) throw new ApiError(envelope.error?.code || 'error', envelope.error?.message || 'Request failed.');
    return envelope.data;
  };
}
