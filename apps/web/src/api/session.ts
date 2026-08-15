export type AuthSession =
  | { authenticated: false }
  | { authenticated: false; refreshable: true; csrfToken: string }
  | { authenticated: true; principal: { displayName: string }; capabilities: { canEditDashboard: boolean; canPublishDashboard: boolean }; expiresAt: string; refreshAt: string; csrfToken: string };

export function parseAuthSession(value: unknown): AuthSession {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("invalid session");
  const record = value as Record<string, unknown>;
	if (record.authenticated === false && Object.keys(record).length === 1) return { authenticated: false };
	if (record.authenticated === false && record.refreshable === true && Object.keys(record).sort().join(",") === "authenticated,csrfToken,refreshable" && typeof record.csrfToken === "string" && /^[A-Za-z0-9_-]{43}$/.test(record.csrfToken)) return value as AuthSession;
  const allowed = new Set(["authenticated", "principal", "capabilities", "expiresAt", "refreshAt", "csrfToken"]);
  if (record.authenticated !== true || Object.keys(record).some((key) => !allowed.has(key))) throw new Error("invalid session");
  const principal = record.principal as Record<string, unknown> | undefined, capabilities = record.capabilities as Record<string, unknown> | undefined;
  if (!principal || Object.keys(principal).length !== 1 || typeof principal.displayName !== "string" || principal.displayName.length < 1 || principal.displayName.length > 128) throw new Error("invalid session");
  if (!capabilities || Object.keys(capabilities).sort().join(",") !== "canEditDashboard,canPublishDashboard" || typeof capabilities.canEditDashboard !== "boolean" || typeof capabilities.canPublishDashboard !== "boolean") throw new Error("invalid session");
  if (typeof record.csrfToken !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(record.csrfToken) || typeof record.expiresAt !== "string" || typeof record.refreshAt !== "string") throw new Error("invalid session");
  const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
  if (!rfc3339.test(record.refreshAt) || !rfc3339.test(record.expiresAt)) throw new Error("invalid session");
  const refresh = Date.parse(record.refreshAt), expires = Date.parse(record.expiresAt);
  // The server is the authorization-time authority. A due refreshAt remains a
  // valid authenticated response and is scheduled immediately by AuthGate.
  if (!Number.isFinite(refresh) || !Number.isFinite(expires) || refresh >= expires) throw new Error("invalid session");
  return value as AuthSession;
}

export async function readAuthSession(response: Response, signal?: AbortSignal): Promise<AuthSession> {
  const contentType = response.headers.get("content-type") ?? "";
  if (contentType.split(";", 1)[0]?.trim().toLowerCase() !== "application/json") throw new Error("invalid session");
  const declared = Number(response.headers.get("content-length")); if (Number.isFinite(declared) && declared > 4096) throw new Error("invalid session");
  if (!response.body) throw new Error("invalid session");
  const reader = response.body.getReader(), chunks: Uint8Array[] = []; let total = 0;
  const abort = () => { void reader.cancel(signal?.reason); };
  if (signal?.aborted) abort(); else signal?.addEventListener("abort", abort, { once: true });
  try { for (;;) { const { done, value } = await reader.read(); if (signal?.aborted) throw signal.reason; if (done) break; total += value.byteLength; if (total > 4096) { await reader.cancel(); throw new Error("invalid session"); } chunks.push(value); } }
  finally { signal?.removeEventListener("abort", abort); reader.releaseLock(); }
  const bytes = new Uint8Array(total); let offset = 0; for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  return parseAuthSession(JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)) as unknown);
}
