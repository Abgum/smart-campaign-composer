// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./http";

afterEach(() => vi.unstubAllGlobals());

function answer(body: unknown) {
  const calls: string[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    calls.push(String(input));
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  });
  return calls;
}

describe("api", () => {
  it("creates a manual campaign in the current chat without starting a turn", async () => {
    let sent: unknown;
    let url = "";
    vi.stubGlobal(
      "fetch",
      async (input: RequestInfo | URL, init?: RequestInit) => {
        url = String(input);
        sent = JSON.parse(String(init?.body));
        return new Response(
          JSON.stringify({
            campaign_id: "c7",
            chat_id: "chat1",
            version: 1,
            version_id: 3,
          }),
          { status: 200 },
        );
      },
    );
    const rule = {
      match: "any" as const,
      conditions: [
        {
          kind: "attribute" as const,
          field: "language",
          operator: "eq",
          values: ["tr"],
        },
      ],
    };
    await api.createCampaign({
      chat_id: "chat1",
      name: "Manuel taslak",
      segment_rule: rule,
      sms: { text: "Merhaba" },
      email: null,
    });
    expect(url).toBe("/api/campaigns");
    expect(sent).toMatchObject({
      chat_id: "chat1",
      segment_rule: rule,
      sms: { text: "Merhaba" },
    });
  });

  it("sends a changed audience rule with the draft version edit", async () => {
    let sent: any;
    vi.stubGlobal(
      "fetch",
      async (_input: RequestInfo | URL, init?: RequestInit) => {
        sent = JSON.parse(String(init?.body));
        return new Response(
          JSON.stringify({ campaign_id: "c7", version: 2, version_id: 4 }),
          { status: 200 },
        );
      },
    );
    const rule = {
      match: "all" as const,
      conditions: [
        {
          kind: "event" as const,
          event_type: "cart_add",
          within_days: 30,
          not_followed_by: "purchase",
        },
      ],
    };
    await api.editCampaign("c7", {
      base_version_id: 3,
      name: "Taslak",
      segment_rule: rule,
      sms: null,
      email: null,
    });
    expect(sent.segment_rule).toEqual(rule);
  });

  it("addresses campaign reads, edits, and approval by public UUID", async () => {
    const id = "00000000-0000-4000-8000-000000000007";
    const calls = answer({ id, campaign_id: id });
    await api.campaign(id);
    await api.editCampaign(id, {
      base_version_id: 31,
      name: "İstanbul sepet",
      sms: null,
      email: null,
    });
    await api.approve(id, 31);
    await api.cancelScheduled(id);
    expect(calls).toEqual([
      `/api/campaigns/${id}`,
      `/api/campaigns/${id}/versions`,
      `/api/campaigns/${id}/approve`,
      `/api/campaigns/${id}/cancel`,
    ]);
  });

  it("lists recent chats from GET /api/chats {chats}", async () => {
    const calls = answer({
      chats: [
        {
          id: "c1",
          title: "İstanbul sepet",
          updated_at: "2026-10-27T10:00:00Z",
        },
      ],
    });
    const chats = await api.chats(5);
    expect(calls).toEqual(["/api/chats?limit=5"]);
    expect(chats.map((c) => c.title)).toEqual(["İstanbul sepet"]);
  });
});
