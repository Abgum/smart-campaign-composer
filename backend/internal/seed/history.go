package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/segment"
)

// HistoryModel names the demo model in the history's messages and versions,
// so the UI shows it as mock output, apart from the live demo model.
const HistoryModel = "demo-history"

// historyChat is the id of history chat n; the first one marks the history
// as loaded.
func historyChat(n int) string { return fmt.Sprintf("5eed0000-0000-4000-8000-%012d", n) }

// pastCampaign is one campaign of the history: the request that created it,
// maybe a later request that changed it, and its approval.
type pastCampaign struct {
	chat    int    // history chat number, from 1
	day     int    // days before today
	clock   string // local time of the request, 15:04
	ask     string // what the marketer wrote
	draft   pastDraft
	revise  *pastRevision
	approve time.Duration // after the last request; 0 keeps the draft
}

type pastRevision struct {
	after time.Duration
	ask   string
	draft pastDraft
}

type pastDraft struct {
	name, summary string
	rule          segment.Rule
	sms           *content.SMS
	email         *content.Email
}

// InsertHistory adds six weeks of past campaigns to a database that has its
// recipients: chats in which marketers asked for campaigns, the drafts the
// demo model wrote and approvals on many days. It reports whether it did; a
// database that has the history already is left untouched.
//
// It goes through the app's own draft and approval code with the clock set
// back, so audiences, consent exclusions, duplicate skips and city snapshots
// are what the app writes. Rules read today's recipients (orders, spend,
// language); sign-up windows and events are measured at the approval time.
// It sends nothing: the approved deliveries are due, and the worker sends
// them through the provider like any others, all or none of them, since
// everything is written in one transaction. ShiftHistory then moves the
// times it recorded to the approval days.
func InsertHistory(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, now time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('scc-seed-history'))`); err != nil {
		return false, err
	}
	var loaded, recipients bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chats WHERE id = $1), EXISTS (SELECT 1 FROM users)`,
		historyChat(1)).Scan(&loaded, &recipients); err != nil {
		return false, err
	}
	if loaded {
		return false, nil
	}
	if !recipients {
		return false, errors.New("seed history: no recipients; run seed first")
	}

	h := &history{tx: tx, cat: segment.DefaultCatalog(), store: campaign.NewStore(tx), lastActive: map[int]time.Time{}}
	today := now.In(loc)
	past := slices.Clone(pastCampaigns)
	at := func(c pastCampaign) time.Time {
		clock, _ := time.Parse("15:04", c.clock)
		return time.Date(today.Year(), today.Month(), today.Day()-c.day, clock.Hour(), clock.Minute(), 0, 0, loc)
	}
	slices.SortStableFunc(past, func(a, b pastCampaign) int { return at(a).Compare(at(b)) })
	for _, c := range past {
		if err := h.add(ctx, c, at(c)); err != nil {
			return false, fmt.Errorf("seed history %q: %w", c.draft.name, err)
		}
	}
	for chat, last := range h.lastActive {
		if _, err := tx.Exec(ctx, `UPDATE chats SET updated_at = $2 WHERE id = $1`, historyChat(chat), last); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type history struct {
	tx         pgx.Tx
	cat        segment.Catalog
	store      *campaign.Store
	lastActive map[int]time.Time // per chat
}

// add writes one campaign: its chat if new, the requests and drafts, then
// the approval.
func (h *history) add(ctx context.Context, c pastCampaign, at time.Time) error {
	chat := historyChat(c.chat)
	if _, err := h.tx.Exec(ctx, `INSERT INTO chats (id, created_at, updated_at) VALUES ($1, $2, $2) ON CONFLICT (id) DO NOTHING`,
		chat, at); err != nil {
		return err
	}
	saved, err := h.request(ctx, chat, at, c.ask, nil, c.draft)
	if err != nil {
		return err
	}
	last := at
	if r := c.revise; r != nil {
		last = at.Add(r.after)
		if saved, err = h.request(ctx, chat, last, r.ask, &saved, r.draft); err != nil {
			return err
		}
	}
	h.lastActive[c.chat] = last
	if c.approve == 0 {
		return nil
	}
	approvedAt := last.Add(c.approve)
	if _, err := h.store.Approve(ctx, saved.CampaignID, saved.VersionID, h.cat, approvedAt); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	h.lastActive[c.chat] = approvedAt
	return nil
}

// request writes one turn of a chat: the marketer's message, the draft the
// demo model saved (a new campaign, or a version of base) and its reply.
func (h *history) request(ctx context.Context, chat string, at time.Time, ask string, base *campaign.Saved, d pastDraft) (campaign.Saved, error) {
	if issues := content.ValidateDraft(d.sms, d.email); len(issues) > 0 {
		return campaign.Saved{}, fmt.Errorf("content: %v", issues)
	}
	checked := segment.Validate(d.rule, h.cat)
	if !checked.OK() {
		return campaign.Saved{}, fmt.Errorf("rule: %v", checked.Errors)
	}
	audience, err := segment.Count(ctx, h.tx, checked.Normalized, h.cat, at)
	if err != nil {
		return campaign.Saved{}, err
	}

	route, focus := map[string]any{"intent": "create", "source": "router", "allow_create": true, "targets": []any{}}, (*int64)(nil)
	cmd := campaign.DraftCmd{Name: d.name, Rule: checked.Normalized, SMS: d.sms, Email: d.email, Audience: audience, ChangeSummary: d.summary}
	if base != nil {
		route = map[string]any{"intent": "edit", "source": "router", "allow_create": false,
			"targets": []any{map[string]any{"campaign_id": base.CampaignID, "ordinal": base.Ordinal, "name": d.name}}}
		focus, cmd.CampaignID, cmd.BaseVersion = &base.CampaignID, &base.CampaignID, &base.Version
	}
	routeJSON, _ := json.Marshal(route)
	replied := at.Add(20 * time.Second)
	var turn string
	if err := h.tx.QueryRow(ctx, `
		INSERT INTO turns (chat_id, client_turn_id, request_hash, status, outcome, route, focus_campaign_id, heartbeat_at, started_at, finished_at)
		VALUES ($1, gen_random_uuid(), 'demo-history', 'completed', 'ok', $2, $3, $5, $4, $5)
		RETURNING id::text`, chat, routeJSON, focus, at, replied).Scan(&turn); err != nil {
		return campaign.Saved{}, err
	}
	if _, err := h.tx.Exec(ctx, `INSERT INTO messages (chat_id, turn_id, role, text, created_at) VALUES ($1, $2, 'user', $3, $4)`,
		chat, turn, ask, at); err != nil {
		return campaign.Saved{}, err
	}

	saved, err := h.store.Drafts(campaign.Author{ChatID: chat, TurnID: turn, Source: "llm", Provider: "mock", Model: HistoryModel}).
		SaveDraft(ctx, cmd)
	if err != nil {
		return campaign.Saved{}, fmt.Errorf("save draft: %w", err)
	}
	if _, err := h.tx.Exec(ctx, `UPDATE campaign_versions SET created_at = $2 WHERE id = $1`, saved.VersionID, replied); err != nil {
		return campaign.Saved{}, err
	}
	if _, err := h.tx.Exec(ctx, `
		UPDATE campaigns SET created_at = CASE WHEN $3 THEN $2 ELSE created_at END, updated_at = $2 WHERE id = $1`,
		saved.CampaignID, replied, saved.Created); err != nil {
		return campaign.Saved{}, err
	}

	reply := fmt.Sprintf("Taslak hazır: %s.\nKampanya %d v%d kaydedildi (yeni kampanya).", d.name, saved.Ordinal, saved.Version)
	if !saved.Created {
		reply = fmt.Sprintf("Değişiklik kaydedildi.\nKampanya %d v%d kaydedildi (%s).", saved.Ordinal, saved.Version, d.summary)
	}
	reply += fmt.Sprintf(" Kitle: %d kişi; SMS'e uygun %d, e-postaya uygun %d.", audience.Total, audience.SMSReachable, audience.EmailReachable)
	if _, err := h.tx.Exec(ctx, `
		INSERT INTO messages (chat_id, turn_id, role, text, outcome, llm_provider, llm_model, created_at)
		VALUES ($1, $2, 'assistant', $3, 'ok', 'mock', $4, $5)`, chat, turn, reply, HistoryModel, replied); err != nil {
		return campaign.Saved{}, err
	}
	return saved, nil
}

// historyChats are the ids of every history chat.
func historyChats() []string {
	var ids []string
	for _, c := range pastCampaigns {
		if id := historyChat(c.chat); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// HistoryLeft counts what of the history the worker has yet to finish: its
// deliveries still waiting or in flight, and its approved campaigns not yet
// settled as completed.
func HistoryLeft(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var left int
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM deliveries d JOIN campaigns c ON c.id = d.campaign_id
		        WHERE c.chat_id = ANY($1::uuid[]) AND d.status IN ('pending', 'retry', 'in_flight'))
		     + (SELECT count(*) FROM campaigns WHERE chat_id = ANY($1::uuid[]) AND status IN ('approved', 'sending'))`,
		historyChats()).Scan(&left)
	return left, err
}

// shiftLead is how long after approval a history campaign's first
// recorded terminal result lands once shifted.
const shiftLead = 3 * time.Second

// ShiftHistory moves the times the worker recorded for each completed
// history campaign back to its approval day, by one offset per campaign: the
// first recorded terminal result lands shiftLead after approval, and gaps
// between terminal results stay as the worker recorded them. These timestamps
// are not provider call starts; the live worker enforces the channel limit.
// It reports how many campaigns it moved; a result within an hour of its
// approval counts as moved already.
func ShiftHistory(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('scc-seed-history'))`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.approved_at, min(coalesce(d.sent_at, d.finished_at))
		FROM campaigns c JOIN deliveries d ON d.campaign_id = c.id
		WHERE c.chat_id = ANY($1::uuid[]) AND c.status = 'completed'
		GROUP BY c.id, c.approved_at
		HAVING min(coalesce(d.sent_at, d.finished_at)) > c.approved_at + interval '1 hour'`, historyChats())
	if err != nil {
		return 0, err
	}
	type sent struct {
		ID                int64
		Approved, FirstAt time.Time
	}
	campaigns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sent])
	if err != nil {
		return 0, err
	}
	for _, c := range campaigns {
		by := c.Approved.Add(shiftLead).Sub(c.FirstAt)
		if _, err := tx.Exec(ctx, `
			UPDATE deliveries SET sent_at = sent_at + $2::interval, finished_at = finished_at + $2::interval,
			       updated_at = updated_at + $2::interval,
			       next_attempt_at = CASE WHEN next_attempt_at > $3 THEN next_attempt_at + $2::interval ELSE next_attempt_at END,
			       created_at = $3
			WHERE campaign_id = $1`, c.ID, by, c.Approved); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE campaigns SET updated_at = (SELECT max(coalesce(finished_at, sent_at)) FROM deliveries WHERE campaign_id = $1)
			WHERE id = $1`, c.ID); err != nil {
			return 0, err
		}
	}
	return len(campaigns), tx.Commit(ctx)
}

// Rule and content helpers for the campaigns below.

func all(cs ...segment.Condition) segment.Rule { return segment.Rule{Match: "all", Conditions: cs} }

func attr(field, op string, values ...string) segment.Condition {
	return segment.Condition{Kind: "attribute", Field: field, Operator: op, Values: values}
}

func inCity(ids ...int64) segment.Condition {
	places := make([]segment.PlaceRef, len(ids))
	for i, id := range ids {
		places[i] = segment.PlaceRef{Level: "city", GeonameID: id}
	}
	return segment.Condition{Kind: "location", Operator: "in", Places: places}
}

func notFollowed(event, next string, days int) segment.Condition {
	return segment.Condition{Kind: "event", EventType: event, WithinDays: days, NotFollowedBy: next}
}

func none(event string, days int) segment.Condition {
	return segment.Condition{Kind: "event", EventType: event, WithinDays: days, Negate: true}
}

func sms(text string) *content.SMS { return &content.SMS{Text: text} }

func email(subject, body string) *content.Email { return &content.Email{Subject: subject, Body: body} }

var (
	ankaraCartSMS   = sms("{{first_name}}, sepetindeki ürünler seni bekliyor. Bugün tamamlarsan kargo bizden!")
	ankaraCartEmail = email("Sepetin seni bekliyor",
		"Merhaba {{first_name}},\n\nSepetine eklediğin ürünler hâlâ seni bekliyor. Siparişini bugün tamamlarsan kargo bizden.\n\nİyi alışverişler!")
	antalyaEmail = email("Antalya'ya özel yaz sonu indirimi",
		"Merhaba {{first_name}},\n\nYaz bitmeden Antalya'ya özel seçili ürünlerde %20 indirim seni bekliyor. Kampanya pazar akşamı sona eriyor.\n\nİyi alışverişler!")
	novemberSMS  = sms("{{first_name}}, Kasım indirimleri yaklaşıyor! İlk haberi alanlardan ol, listeyi kaçırma.")
	novemberRule = all(attr("total_orders", "gte", "1"))
)

// pastCampaigns is the history: six chats over six weeks. The second Ankara
// wave repeats the first one's text, so people who got it are skipped as
// duplicates; the last one waits for approval.
var pastCampaigns = []pastCampaign{
	{chat: 1, day: 43, clock: "10:15", approve: 35 * time.Minute,
		ask: "Son bir ayda üye olanlara hoş geldin mesajı gönderelim, SMS ve e-posta olsun.",
		draft: pastDraft{name: "Yeni üyelere hoş geldin", summary: "İlk taslak",
			rule: all(attr("signup_days_ago", "between", "0", "30")),
			sms:  sms("Merhaba {{first_name}}, aramıza hoş geldin! İlk siparişinde HOSGELDIN koduyla %10 indirim seni bekliyor."),
			email: email("Hoş geldin {{first_name}}!",
				"Merhaba {{first_name}},\n\nAramıza katıldığın için teşekkürler. İlk siparişinde HOSGELDIN koduyla %10 indirim kazanırsın.\n\nİyi alışverişler!")}},
	{chat: 2, day: 39, clock: "19:05", approve: 20 * time.Minute,
		ask: "Ankara'da son iki haftada sepete ürün ekleyip satın almayanlara hatırlatma hazırla.",
		draft: pastDraft{name: "Ankara sepet hatırlatma", summary: "İlk taslak",
			rule: all(inCity(323786), notFollowed("cart_add", "purchase", 14)), sms: ankaraCartSMS, email: ankaraCartEmail}},
	{chat: 3, day: 35, clock: "09:40", approve: 50 * time.Minute,
		ask: "En az iki sipariş vermiş müşterilere teşekkür e-postası yaz.",
		draft: pastDraft{name: "Sadık müşterilere teşekkür", summary: "İlk taslak",
			rule: all(attr("total_orders", "gte", "2")),
			email: email("Teşekkürler {{first_name}}",
				"Merhaba {{first_name}},\n\nBizi tercih ettiğin için teşekkür ederiz. Bir sonraki siparişinde TESEKKUR koduyla %15 indirim senin.\n\nSevgiyle")}},
	{chat: 1, day: 31, clock: "11:20", approve: 40 * time.Minute,
		ask: "Üye olalı bir ila üç hafta olmuş ama hiç sipariş vermemiş kişilere e-posta hazırla.",
		draft: pastDraft{name: "İlk sipariş hatırlatma", summary: "İlk taslak",
			rule: all(attr("signup_days_ago", "between", "7", "21"), attr("total_orders", "eq", "0")),
			email: email("{{first_name}}, hoş geldin indirimin hâlâ geçerli",
				"Merhaba {{first_name}},\n\nHoş geldin indirimin hâlâ geçerli: HOSGELDIN koduyla ilk siparişinde %10 indirim.\n\nGörüşmek üzere!")}},
	{chat: 2, day: 30, clock: "18:30", approve: 15 * time.Minute,
		ask: "Ankara sepet hatırlatmasını son 30 günü kapsayacak şekilde tekrar gönderelim, metin aynı kalsın.",
		draft: pastDraft{name: "Ankara sepet hatırlatma, 2. dalga", summary: "İlk taslak",
			rule: all(inCity(323786), notFollowed("cart_add", "purchase", 30)), sms: ankaraCartSMS, email: ankaraCartEmail}},
	{chat: 4, day: 27, clock: "12:30", approve: 30 * time.Minute,
		ask: "Antalya'daki müşterilere yaz sonu kampanyası hazırla, SMS ve e-posta.",
		draft: pastDraft{name: "Antalya yaz sonu", summary: "İlk taslak", rule: all(inCity(323777)),
			sms:   sms("Yaz bitmeden {{first_name}}! Antalya'ya özel seçili ürünlerde %20 indirim bu hafta sonu."),
			email: antalyaEmail},
		revise: &pastRevision{after: 12 * time.Minute, ask: "SMS'e YAZSONU indirim kodunu ekle.",
			draft: pastDraft{name: "Antalya yaz sonu", summary: "SMS'e YAZSONU kodu eklendi", rule: all(inCity(323777)),
				sms:   sms("Yaz bitmeden {{first_name}}! Antalya'ya özel seçili ürünlerde YAZSONU koduyla %20 indirim bu hafta sonu."),
				email: antalyaEmail}}},
	{chat: 5, day: 23, clock: "10:05", approve: 45 * time.Minute,
		ask: "Son iki ayda hiç ürüne bakmamış müşterileri geri kazanmak için e-posta hazırla.",
		draft: pastDraft{name: "Seni özledik", summary: "İlk taslak", rule: all(none("product_view", 60)),
			email: email("Seni özledik {{first_name}}",
				"Merhaba {{first_name}},\n\nBir süredir uğramadın. Yeni sezon ürünlerine göz atmak için güzel bir zaman: GERIDON koduyla %15 indirim seni bekliyor.\n\nGörüşmek üzere!")}},
	{chat: 2, day: 19, clock: "20:10", approve: 10 * time.Minute,
		ask: "İzmir'de son 30 günde sepeti terk edenlere sadece SMS gönder.",
		draft: pastDraft{name: "İzmir sepet hatırlatma", summary: "İlk taslak",
			rule: all(inCity(311046), notFollowed("cart_add", "purchase", 30)),
			sms:  sms("{{first_name}}, İzmir'e kargo bugün ücretsiz! Sepetindeki ürünleri kaçırma.")}},
	{chat: 3, day: 15, clock: "14:00", approve: 25 * time.Minute,
		ask: "Toplam harcaması 1500 TL'yi geçen müşterilere VIP indirim kodu gönderelim.",
		draft: pastDraft{name: "VIP indirim kodu", summary: "İlk taslak", rule: all(attr("total_spent", "gte", "1500")),
			sms: sms("{{first_name}}, sana özel VIP15 koduyla tüm ürünlerde %15 indirim. Son gün pazar!"),
			email: email("{{first_name}}, VIP indirimin hazır",
				"Merhaba {{first_name}},\n\nEn değerli müşterilerimizden biri olduğun için VIP15 koduyla tüm ürünlerde %15 indirim tanımladık. Kod pazar gecesine kadar geçerli.\n\nTeşekkürler!")}},
	{chat: 4, day: 11, clock: "18:20", approve: 20 * time.Minute,
		ask: "Bursa'daki müşterilere hafta sonu fırsatını duyuralım.",
		draft: pastDraft{name: "Bursa hafta sonu fırsatı", summary: "İlk taslak", rule: all(inCity(750269)),
			sms: sms("{{first_name}}, Bursa'ya özel hafta sonu fırsatı: seçili ürünlerde 2 al 1 öde!"),
			email: email("Bursa'ya özel hafta sonu fırsatı",
				"Merhaba {{first_name}},\n\nBu hafta sonu Bursa'ya özel seçili ürünlerde 2 al 1 öde fırsatı var.\n\nİyi alışverişler!")}},
	{chat: 5, day: 8, clock: "16:45", approve: 15 * time.Minute,
		ask: "Son bir haftada ürünlere bakıp sepete eklemeyenlere SMS at.",
		draft: pastDraft{name: "Bakıp almayanlar", summary: "İlk taslak", rule: all(notFollowed("product_view", "cart_add", 7)),
			sms: sms("{{first_name}}, göz attığın ürünler tükenmeden yakala! Bugüne özel kargo bedava.")}},
	{chat: 6, day: 5, clock: "11:00", approve: 30 * time.Minute,
		ask: "İngilizce tercih eden kullanıcılara İngilizce bir bülten e-postası hazırla.",
		draft: pastDraft{name: "English newsletter", summary: "İlk taslak", rule: all(attr("language", "eq", "en")),
			email: email("New arrivals for you, {{first_name}}",
				"Hi {{first_name}},\n\nOur new season collection is here. Use WELCOME10 for 10% off your next order.\n\nHappy shopping!")}},
	{chat: 6, day: 3, clock: "13:15", approve: 40 * time.Minute,
		ask: "İstanbul'da 25-40 yaş arası müşterilere hafta içi kampanyası hazırla.",
		draft: pastDraft{name: "İstanbul hafta içi", summary: "İlk taslak",
			rule: all(inCity(745044), attr("age", "between", "25", "40")),
			sms:  sms("{{first_name}}, hafta içi İstanbul'a özel: 500 TL üzeri alışverişte 75 TL indirim!"),
			email: email("Hafta içine özel İstanbul indirimi",
				"Merhaba {{first_name}},\n\nHafta içi boyunca İstanbul'a özel 500 TL üzeri alışverişlerde 75 TL indirim seni bekliyor.\n\nİyi alışverişler!")}},
	{chat: 4, day: 1, clock: "10:30", approve: 25 * time.Minute,
		ask: "İzmir, Muğla ve Denizli'deki müşterilere Ege'ye özel e-posta gönderelim.",
		draft: pastDraft{name: "Ege'ye özel indirim", summary: "İlk taslak", rule: all(inCity(311046, 304184, 317109)),
			email: email("Ege'ye özel indirim, {{first_name}}",
				"Merhaba {{first_name}},\n\nEge'deki müşterilerimize özel seçili ürünlerde %15 indirim başladı.\n\nİyi alışverişler!")}},
	{chat: 3, day: 1, clock: "15:00",
		ask: "Kasım indirimi için siparişi olan müşterilere ön duyuru hazırla.",
		draft: pastDraft{name: "Kasım indirimi ön duyuru", summary: "İlk taslak", rule: novemberRule, sms: novemberSMS,
			email: email("{{first_name}}, Kasım indirimleri çok yakında! Fırsatları ilk sen öğren",
				"Merhaba {{first_name}},\n\nKasım indirimleri başlamak üzere. Fırsatları ilk öğrenenlerden olmak için bizi takip etmeye devam et.\n\nGörüşmek üzere!")},
		revise: &pastRevision{after: 8 * time.Minute, ask: "E-posta konusunu kısalt.",
			draft: pastDraft{name: "Kasım indirimi ön duyuru", summary: "E-posta konusu kısaltıldı", rule: novemberRule, sms: novemberSMS,
				email: email("Kasım indirimleri çok yakında",
					"Merhaba {{first_name}},\n\nKasım indirimleri başlamak üzere. Fırsatları ilk öğrenenlerden olmak için bizi takip etmeye devam et.\n\nGörüşmek üzere!")}}},
}
