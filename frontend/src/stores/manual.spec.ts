import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useChatStore } from "./chat";

beforeEach(() => setActivePinia(createPinia()));
afterEach(() => vi.unstubAllGlobals());

const id = "00000000-0000-4000-8000-000000000007";
const draft = {
  name: "Manuel kampanya",
  segment_rule: {
    match: "all" as const,
    conditions: [
      {
        kind: "attribute" as const,
        field: "age",
        operator: "gte",
        values: ["18"],
      },
    ],
  },
  sms: { text: "Merhaba" },
  email: null,
};

describe("manual campaigns in a chat", () => {
  it("creates in the open chat and focuses the new draft without sending a turn", async () => {
    const calls: { url: string; body?: any }[] = [];
    vi.stubGlobal(
      "fetch",
      async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        calls.push({
          url,
          body: init?.body ? JSON.parse(String(init.body)) : undefined,
        });
        const data =
          url === "/api/campaigns"
            ? { campaign_id: id, chat_id: "c1", version: 1, version_id: 8 }
            : url === "/api/chats/c1"
              ? {
                  id: "c1",
                  messages: [],
                  campaigns: [
                    {
                      id,
                      ordinal: 1,
                      name: "Manuel kampanya",
                      status: "draft",
                      version: 1,
                    },
                  ],
                }
              : {
                  id,
                  chat_id: "c1",
                  ordinal: 1,
                  name: "Manuel kampanya",
                  status: "draft",
                  current_version: null,
                  versions: [],
                };
        return new Response(JSON.stringify(data), { status: 200 });
      },
    );
    const store = useChatStore();
    store.chatId = "c1";
    await store.createManual(draft);
    expect(calls.find((c) => c.url === "/api/campaigns")?.body).toMatchObject({
      chat_id: "c1",
      name: "Manuel kampanya",
    });
    expect(calls.some((c) => c.url.includes("/turns"))).toBe(false);
    expect(store.focusId).toBe(id);
    expect(store.campaigns[0]?.id).toBe(id);
  });

  it("does not retry a committed create when refreshing the chat fails", async () => {
    let posts = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/campaigns") {
        posts++;
        return new Response(
          JSON.stringify({
            campaign_id: id,
            chat_id: "c1",
            ordinal: 2,
            version: 1,
            version_id: 8,
          }),
          { status: 200 },
        );
      }
      return new Response(
        JSON.stringify({ error: "offline", message: "Yenileme başarısız" }),
        { status: 503 },
      );
    });
    const store = useChatStore();
    store.chatId = "c1";
    const created = await store.createManual(draft);
    expect(created.campaign_id).toBe(id);
    expect(store.focusId).toBe(id);
    expect(store.notice?.text).toContain("kaydedildi");
    expect(store.notice?.text).toContain("yenilenemedi");
    expect(posts).toBe(1);
  });

  it("keeps the current form available when a stale edit is rejected", async () => {
    const calls: string[] = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      calls.push(String(input));
      return new Response(
        JSON.stringify({
          error: "stale_version",
          message: "Kampanya değişti",
          current_version: 3,
        }),
        { status: 409 },
      );
    });
    const store = useChatStore();
    store.chatId = "c1";
    await expect(store.saveEdit(id, 2, draft)).rejects.toThrow(
      "Kampanya değişti",
    );
    expect(calls).toEqual([`/api/campaigns/${id}/versions`]);
  });

  it("does not report a committed edit as failed when refreshing its view fails", async () => {
    let edits = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === `/api/campaigns/${id}/versions`) {
        edits++;
        return new Response(
          JSON.stringify({ campaign_id: id, version: 3, version_id: 18 }),
          { status: 200 },
        );
      }
      return new Response(
        JSON.stringify({ error: "offline", message: "Yenileme başarısız" }),
        { status: 503 },
      );
    });
    const store = useChatStore();
    store.chatId = "c1";
    await store.saveEdit(id, 17, draft);
    expect(edits).toBe(1);
    expect(store.notice?.text).toContain("kaydedildi");
    expect(store.notice?.text).toContain("yenilenemedi");
  });
});
