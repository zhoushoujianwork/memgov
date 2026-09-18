// The UI talks only to this client. Desktop transports can replace it later.
export async function query(path, options = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10000);
  let response, payload;
  try {
    response = await fetch(`/api/v1/${path}`, {
      credentials: "same-origin",
      cache: "no-store",
      signal: controller.signal,
      ...options,
    });
    payload = await response.json();
  } finally {
    clearTimeout(timeout);
  }
  if (!response.ok || !payload.ok)
    throw new Error(
      payload.error?.message || `本地请求失败 (${response.status})`,
    );
  return payload;
}
// Older startup URLs may still contain a token fragment; no login is needed.
export function clearLegacyToken() {
  if (new URLSearchParams(location.hash.slice(1)).has("token")) {
    history.replaceState(null, "", location.pathname + location.search);
  }
}
