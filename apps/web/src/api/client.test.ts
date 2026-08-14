import { describe, expect, it, vi } from "vitest";
import { apiGet, HttpError } from "./client";

describe("apiGet", () => {
  it("forwards cancellation and uses only the BFF path", async () => {
    const controller = new AbortController();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: { "content-type": "application/json" } }),
    );

    await expect(apiGet("/api/v1/scope", {}, controller.signal)).resolves.toEqual({ ok: true });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(new URL(String(url)).pathname).toBe("/api/v1/scope");
    expect(init?.signal).toBe(controller.signal);
    expect(init?.headers).toEqual({ accept: "application/json" });
  });

  it("preserves a structured forbidden response", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ code: "forbidden", message: "denied", requestId: "req-1" }), {
        status: 403,
        headers: { "content-type": "application/json" },
      }),
    );

    const error = await apiGet("/api/v1/scope").catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(HttpError);
    expect((error as HttpError).status).toBe(403);
    expect((error as HttpError).body.code).toBe("forbidden");
  });
});
