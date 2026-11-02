import { afterEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createMemoryHistory, createRouter } from "vue-router";
import { routes } from "../router";
import CampaignDetailView from "./CampaignDetailView.vue";
import CampaignsView from "./CampaignsView.vue";
import NewCampaignView from "./NewCampaignView.vue";
import CampaignTable from "../components/overview/CampaignTable.vue";
import type {
  Analytics,
  CampaignDetail,
  Counts,
  TrackedCampaign,
} from "../api/types";

const campaignId = "00000000-0000-4000-8000-000000000007";
const missingId = "00000000-0000-4000-8000-000000000099";

const counts = (c: Partial<Counts>): Counts => ({
  excluded_consent: 0,
  excluded_duplicate: 0,
  pending: 0,
  in_flight: 0,
  retry: 0,
  sent: 0,
  failed: 0,
  unknown: 0,
  skipped_no_consent: 0,
  ...c,
});

const tracked: TrackedCampaign = {
  id: campaignId,
  chat_id: "c1",
  ordinal: 2,
  name: "İstanbul sepet",
  status: "sending",
  updated_at: "2026-10-27T10:00:00Z",
  approved_at: "2026-10-27T09:00:00Z",
  last_sent_at: "2026-10-27T10:00:00Z",
  version: 3,
  llm_provider: "mock",
  channels: {
    sms: counts({
      sent: 120,
      failed: 2,
      unknown: 1,
      pending: 52,
      excluded_consent: 79,
    }),
    email: counts({ sent: 130, pending: 65, excluded_duplicate: 3 }),
  },
};

const detail = (d: Partial<CampaignDetail> = {}): CampaignDetail => ({
  id: campaignId,
  chat_id: "c1",
  ordinal: 2,
  name: "İstanbul sepet",
  status: "sending",
  current_version: {
    id: 31,
    version: 3,
    source: "llm",
    llm_provider: "mock",
    name: "İstanbul sepet",
    segment_rule: {
      match: "all",
      conditions: [
        {
          kind: "location",
          operator: "in",
          places: [{ level: "admin1", name: "İstanbul" }],
        },
      ],
    },
    sms: { text: "Merhaba {{first_name}}, sepetin seni bekliyor!" },
    email: {
      subject: "Sepetin seni bekliyor",
      body: "Merhaba {{first_name}},\n\nKargo bizden.",
    },
    audience: { total: 400, sms_reachable: 254, email_reachable: 198 },
    change_summary: "Son gün eklendi",
    created_at: "2026-10-27T08:00:00Z",
  },
  approved_version_id: 31,
  approval: {
    sms: { selected: 175, excluded_consent: 79, excluded_duplicate: 0 },
    email: { selected: 195, excluded_consent: 0, excluded_duplicate: 3 },
  },
  versions: [],
  updated_at: "2026-10-27T09:00:00Z",
  ...d,
});

const analytics: Analytics = {
  window: {
    range: "all",
    from: "2026-10-27",
    to: "2026-10-28",
    tz: "Europe/Istanbul",
    bucket: "day",
  },
  kpis: {
    sent: { total: 250, sms: 120, email: 130 },
    queued: {
      total: 117,
      pending: 117,
      in_flight: 0,
      retry: 0,
      sms: 52,
      email: 65,
    },
    failed: { total: 2, sms: 2, email: 0 },
    unknown: { total: 1, sms: 1, email: 0 },
  },
  series: [
    { at: "2026-10-27", sms: 120, email: 130 },
    { at: "2026-10-28", sms: 0, email: 0 },
  ],
  cities: {
    top: [],
    other: { cities: 0, total: 0 },
    unknown: { total: 0, sms: 0, email: 0 },
  },
};

async function router(path: string) {
  const r = createRouter({ history: createMemoryHistory(), routes });
  await r.push(path);
  await r.isReady();
  return r;
}

let calls: string[] = [];
function serve(found: CampaignDetail | null, list: TrackedCampaign[]) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push(url);
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      if (url.startsWith("/api/analytics")) return json(analytics);
      if (url === "/api/campaign-rules/catalog")
        return json({
          fields: [
            {
              name: "age",
              type: "int",
              operators: ["gte"],
              min: 0,
              max: 120,
              description: "Age",
            },
          ],
          events: [],
          languages: [],
          places: [
            { level: "admin1", name: "İstanbul", code: "TR-34", country: "TR" },
          ],
          max_conditions: 10,
          max_within_days: 365,
          max_min_count: 100,
          max_list_values: 20,
        });
      if (url.startsWith("/api/campaigns?")) return json({ campaigns: list });
      if (url.endsWith("/cancel") && init?.method === "POST" && found) {
        found = { ...found, status: "cancelled" };
        return json({ status: "cancelled" });
      }
      if (url.startsWith("/api/campaigns/"))
        return found
          ? json(found)
          : json({ error: "not_found", message: "kampanya yok" }, 404);
      return json({});
    },
  );
}
afterEach(() => vi.unstubAllGlobals());

async function page(path: string) {
  const r = await router(path);
  const pinia = createPinia();
  setActivePinia(pinia);
  const w = mount(CampaignDetailView, { global: { plugins: [r, pinia] } });
  await flushPromises();
  return w;
}

describe("campaign page", () => {
  it("retains an edited form after stale version conflict until the user chooses to reload", async () => {
    const first = detail({
      status: "draft",
      approved_version_id: null,
      approval: null,
    });
    const latest = detail({
      status: "draft",
      approved_version_id: null,
      approval: null,
    });
    latest.current_version = {
      ...first.current_version!,
      id: 32,
      version: 4,
      name: "Başkasının değişikliği",
    };
    let reads = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = String(input);
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      if (url === "/api/campaign-rules/catalog")
        return json({
          fields: [
            {
              name: "age",
              type: "int",
              operators: ["gte"],
              description: "Age",
            },
          ],
          events: [],
          languages: [],
          places: [],
          max_conditions: 10,
          max_within_days: 365,
          max_min_count: 100,
          max_list_values: 20,
        });
      if (url === `/api/campaigns/${campaignId}/versions`)
        return json({ error: "stale_version", message: "Sürüm değişti" }, 409);
      if (url === `/api/campaigns/${campaignId}`) {
        reads++;
        return json(reads === 1 ? first : latest);
      }
      if (url.startsWith("/api/campaigns?")) return json({ campaigns: [] });
      return json({});
    });
    const w = await page(`/campaigns/${campaignId}`);
    await w.get('[data-test="edit-campaign"]').trigger("click");
    await flushPromises();
    await w.get('[data-test="campaign-name"]').setValue("Benim değişikliğim");
    await w.get("form.campaign-editor").trigger("submit");
    await flushPromises();
    expect(w.text()).toContain("Sürüm değişti");
    expect(
      (w.get('[data-test="campaign-name"]').element as HTMLInputElement).value,
    ).toBe("Benim değişikliğim");
    expect(w.find('[data-test="reload-conflict"]').exists()).toBe(true);
    await w.get('[data-test="reload-conflict"]').trigger("click");
    await flushPromises();
    expect(
      (w.get('[data-test="campaign-name"]').element as HTMLInputElement).value,
    ).toBe("Başkasının değişikliği");
    w.unmount();
  });
  it("keeps the detail visible and marks a committed edit when its refresh fails", async () => {
    const first = detail({
      status: "draft",
      approved_version_id: null,
      approval: null,
    });
    const latest = detail({
      status: "draft",
      approved_version_id: null,
      approval: null,
    });
    latest.current_version = {
      ...first.current_version!,
      id: 32,
      version: 4,
      name: "Kaydedilen ad",
    };
    latest.name = "Kaydedilen ad";
    let campaignReads = 0;
    let failRefresh = true;
    let edits = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = String(input);
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      if (url === "/api/campaign-rules/catalog")
        return json({
          fields: [
            {
              name: "age",
              type: "int",
              operators: ["gte"],
              description: "Age",
            },
          ],
          events: [],
          languages: [],
          places: [],
          max_conditions: 10,
          max_within_days: 365,
          max_min_count: 100,
          max_list_values: 20,
        });
      if (url === `/api/campaigns/${campaignId}/versions`) {
        edits++;
        return json({ campaign_id: campaignId, version_id: 32, version: 4 });
      }
      if (url === `/api/campaigns/${campaignId}`) {
        campaignReads++;
        return campaignReads > 1 && failRefresh
          ? json({ error: "offline", message: "Yenileme başarısız" }, 503)
          : json(campaignReads > 1 ? latest : first);
      }
      if (url.startsWith("/api/campaigns?")) return json({ campaigns: [] });
      return json({});
    });
    const w = await page(`/campaigns/${campaignId}`);
    await w.get('[data-test="edit-campaign"]').trigger("click");
    await flushPromises();
    await w.get('[data-test="campaign-name"]').setValue("Kaydedilen ad");
    await w.get("form.campaign-editor").trigger("submit");
    await flushPromises();
    expect(edits).toBe(1);
    expect(w.get("h1").text()).toBe("İstanbul sepet");
    expect(w.text()).toContain("Taslak kaydedildi");
    expect(
      w.get('[data-test="edit-campaign"]').attributes("disabled"),
    ).toBeDefined();
    failRefresh = false;
    await w.get('[data-test="retry-refresh"]').trigger("click");
    await flushPromises();
    expect(w.get("h1").text()).toBe("Kaydedilen ad");
    expect(
      w.get('[data-test="edit-campaign"]').attributes("disabled"),
    ).toBeUndefined();
    expect(edits).toBe(1);
    w.unmount();
  });
  it("identifies a manually created first version as manual creation", async () => {
    const manual = detail({
      status: "draft",
      approved_version_id: null,
      approval: null,
    });
    manual.current_version = {
      ...manual.current_version!,
      source: "user",
      llm_provider: null,
      version: 1,
    };
    serve(manual, [
      { ...tracked, status: "draft", approved_at: null, channels: {} },
    ]);
    const w = await page(`/campaigns/${campaignId}`);
    expect(w.get(".campaign-hero .source").text()).toBe(
      "Manuel olarak oluşturuldu",
    );
  });
  it("lets an approved campaign be viewed but not edited", async () => {
    serve(detail(), [tracked]);
    const w = await page(`/campaigns/${campaignId}`);
    expect(w.find('[data-test="edit-campaign"]').exists()).toBe(false);
  });

  it("offers full editing and a separate approval action on a draft", async () => {
    serve(
      detail({ status: "draft", approved_version_id: null, approval: null }),
      [{ ...tracked, status: "draft", approved_at: null, channels: {} }],
    );
    const w = await page(`/campaigns/${campaignId}`);
    expect(w.get('[data-test="edit-campaign"]').text()).toContain("Düzenle");
    await w.get('[data-test="edit-campaign"]').trigger("click");
    await flushPromises();
    expect(w.find('[data-test="rule-match"]').exists()).toBe(true);
    expect(w.find('[data-test="campaign-name"]').exists()).toBe(true);
    expect(w.find('[data-test="approve-campaign"]').exists()).toBe(true);
  });

  it("has its own address under Kampanyalar", async () => {
    const r = await router(`/campaigns/${campaignId}`);
    expect(r.currentRoute.value.name).toBe("campaign");
    expect(r.currentRoute.value.params.id).toBe(campaignId);
    expect(r.currentRoute.value.meta.area).toBe("campaigns");
  });

  it("shows how an approved campaign is sending: per outcome, who was left out, by day and city, and what it says", async () => {
    serve(detail(), [tracked]);
    const w = await page(`/campaigns/${campaignId}`);
    expect(calls).toContain(`/api/campaigns/${campaignId}`);
    expect(calls).toContain(`/api/campaigns?id=${campaignId}`);
    expect(
      calls.some(
        (c) =>
          c.startsWith("/api/analytics") &&
          c.includes("range=all") &&
          c.includes(`campaign_id=${campaignId}`),
      ),
    ).toBe(true);

    expect(w.get("h1").text()).toBe("İstanbul sepet");
    expect(w.get(".campaign-hero .status-chip").text()).toContain(
      "Gönderiliyor",
    );
    const tiles = Object.fromEntries(
      w
        .findAll(".stat-tile")
        .map((t) => [t.get(".tile-label").text(), t.get(".value").text()]),
    );
    expect(tiles).toEqual({
      Gönderildi: "250",
      Kuyrukta: "117",
      Başarısız: "2",
      Belirsiz: "1",
    });
    expect(w.get(".approval-card").text()).toContain("79 kişi izin yok");
    expect(w.get(".approval-card").text()).toContain("3 kişi içerik tekrarı");
    expect(w.find(".columns-chart").exists()).toBe(true);
    expect(w.get(".sms-preview .sms-bubble").text()).toContain(
      "sepetin seni bekliyor",
    );
    const open = w.findAll("a").find((a) => a.text().includes("Asistanda aç"));
    expect(open?.attributes("href")).toBe(
      `/assistant/c1?campaign=${campaignId}`,
    );
  });

  it("shows a scheduled campaign as scheduled until its time, its messages apart from the queue", async () => {
    const at = "2099-11-03T11:30:00Z";
    serve(detail({ status: "approved", scheduled_at: at }), [
      {
        ...tracked,
        status: "approved",
        scheduled_at: at,
        last_sent_at: null,
        channels: {
          sms: counts({ pending: 175, excluded_consent: 79 }),
          email: counts({ pending: 195, excluded_duplicate: 3 }),
        },
      },
    ]);
    const w = await page(`/campaigns/${campaignId}`);
    expect(w.get(".status-chip").text()).toBe("Zamanlandı");
    expect(w.get('[data-test="scheduled-notice"]').text()).toContain("2099");
    const labels = w.findAll(".stat-tile .tile-label").map((l) => l.text());
    expect(labels).toContain("Zamanlanmış");
    expect(labels).not.toContain("Kuyrukta");
    const nativeConfirm = vi.fn(() => true);
    vi.stubGlobal("confirm", nativeConfirm);
    await w.get('[data-test="cancel-scheduled"]').trigger("click");
    await flushPromises();
    expect(nativeConfirm).not.toHaveBeenCalled();
    const dialog = w.get('[role="alertdialog"]');
    expect(dialog.text()).toContain(
      "Bu kampanyanın zamanlanmış gönderimini iptal etmek istiyor musunuz?",
    );
    expect(calls).not.toContain(`/api/campaigns/${campaignId}/cancel`);
    await dialog.get("button.confirm").trigger("click");
    await flushPromises();
    expect(calls).toContain(`/api/campaigns/${campaignId}/cancel`);
    expect(w.find('[role="alertdialog"]').exists()).toBe(false);
    expect(w.get(".status-chip").text()).toBe("İptal");
  });

  it("shows a draft as waiting for approval, with nothing sent to count", async () => {
    serve(
      detail({ status: "draft", approved_version_id: null, approval: null }),
      [{ ...tracked, status: "draft", approved_at: null, channels: {} }],
    );
    const w = await page(`/campaigns/${campaignId}`);
    expect(w.find(".stat-tile").exists()).toBe(false);
    expect(w.find(".columns-chart").exists()).toBe(false);
    expect(w.text()).toContain("Onay bekliyor");
    expect(calls.some((c) => c.startsWith("/api/analytics"))).toBe(false);
  });

  it("says so when there is no such campaign", async () => {
    serve(null, []);
    const w = await page(`/campaigns/${missingId}`);
    expect(w.text()).toContain("Kampanya bulunamadı");
    expect(
      w
        .findAll("a")
        .find((a) => a.text().includes("Kampanyalar"))
        ?.attributes("href"),
    ).toBe("/campaigns");
  });
});

describe("manual creation", () => {
  it("offers AI or a form and creates a campaign in a new chat without a turn", async () => {
    const r = await router("/campaigns/new");
    const calls: { url: string; body?: any }[] = [];
    vi.stubGlobal(
      "fetch",
      async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        calls.push({
          url,
          body: init?.body ? JSON.parse(String(init.body)) : undefined,
        });
        const data = url.endsWith("/catalog")
          ? {
              fields: [
                {
                  name: "age",
                  type: "int",
                  operators: ["gte"],
                  min: 0,
                  max: 120,
                  description: "Age",
                },
              ],
              events: [],
              languages: [],
              places: [],
              max_conditions: 10,
              max_within_days: 365,
              max_min_count: 100,
              max_list_values: 20,
            }
          : {
              campaign_id: campaignId,
              chat_id: "c1",
              version_id: 9,
              version: 1,
            };
        return new Response(JSON.stringify(data), { status: 200 });
      },
    );
    const w = mount(NewCampaignView, { global: { plugins: [r] } });
    await flushPromises();
    expect(w.text()).toContain("Manuel oluşturma");
    expect(w.text()).toContain(
      "Hedef kitle kuralı ve mesajlarınız doğrulanarak taslak olarak kaydedilir.",
    );
    expect(w.text()).toContain("Asistanla oluştur");
    await w.get('[data-test="campaign-name"]').setValue("Manuel kampanya");
    await w.get('[data-test="add-condition"]').trigger("click");
    await w.get('[data-test="attribute-value-0-0"]').setValue("18");
    await w.get('[data-test="sms-on"]').setValue(true);
    await w.get('[data-test="sms-text"]').setValue("Merhaba");
    await w.get("form.campaign-editor").trigger("submit");
    await flushPromises();
    expect(calls.find((c) => c.url === "/api/campaigns")?.body).toMatchObject({
      name: "Manuel kampanya",
      segment_rule: { conditions: [{ field: "age", values: ["18"] }] },
    });
    expect(calls.some((c) => c.url.includes("/turns"))).toBe(false);
    expect(r.currentRoute.value.path).toBe(`/campaigns/${campaignId}`);
  });
});

describe("campaign list", () => {
  it("opens a campaign on its own page", async () => {
    const w = mount(CampaignTable, {
      props: { campaigns: [tracked] },
      global: { plugins: [await router("/campaigns")] },
    });
    expect(w.get(".campaign-name").attributes("href")).toBe(
      `/campaigns/${campaignId}`,
    );
  });

  it("does not describe an unapproved draft as targeting zero recipients", async () => {
    const draft = {
      ...tracked,
      status: "draft" as const,
      approved_at: null,
      channels: {},
    };
    const w = mount(CampaignTable, {
      props: { campaigns: [draft] },
      global: { plugins: [await router("/campaigns")] },
    });
    const target = w.get('.campaign-row td[data-label="Hedef"]');
    expect(target.text()).toBe("—");
    expect(target.attributes("aria-label")).toBe("Hedef onayda belirlenir");
  });

  it("filters by status from a white filter card, in the URL", async () => {
    serve(null, [tracked]);
    const r = await router("/campaigns");
    const pinia = createPinia();
    setActivePinia(pinia);
    const w = mount(CampaignsView, { global: { plugins: [r, pinia] } });
    await flushPromises();
    expect(w.get("h1").text()).toBe("Kampanyalar");
    const draft = w.get('.filter-card .segmented button[data-status="draft"]');
    await draft.trigger("click");
    await flushPromises();
    expect(r.currentRoute.value.query.status).toBe("draft");
    expect(calls.some((c) => c.includes("status=draft"))).toBe(true);
  });
});
