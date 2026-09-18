import { query } from "./shared/api.js";
import type { Envelope } from "./types";
export function get<T>(
  path: string,
  options?: RequestInit,
): Promise<Envelope<T>> {
  return query(path, options) as Promise<Envelope<T>>;
}
export function post<T>(path: string, body: unknown) {
  return get<T>(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Memgov-Console": "1" },
    body: JSON.stringify(body),
  });
}
