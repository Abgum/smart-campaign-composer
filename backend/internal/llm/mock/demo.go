package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
)

// The tools the demo answers. They are the router's and the agent loop's
// tool names; this package cannot import those packages (their tests import
// this one), and the demo tests in package turns catch any drift.
const (
	routerTool   = "route_request"
	draftTool    = "save_campaign_draft"
	removeABTool = "remove_ab_test"
)

// retryMaxTokens is the token limit the agent loop re-issues a truncated
// call with (agentloop.MaxTokensCeiling): the max_tokens scenario truncates
// only below it.
const retryMaxTokens = 32000

// Scenario is a scripted behaviour of the demo model, picked by putting
// "#scenario:<Name>" in the message.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Scenarios lists what the demo model plays. A message without a tag plays
// "default".
var Scenarios = []Scenario{
	{"default", "Geçerli taslak: yeni kampanya ya da seçili sekmedeki kampanyada değişiklik."},
	{"repair", "İlk taslaktaki SMS 172 karakter; backend reddeder, model onarır."},
	{"unknown_city", "Katalogda olmayan bir şehir; backend reddeder, model kullanıcıya sorar."},
	{"new_campaign", "Kampanyalar varken yeni bir kampanya."},
	{"duplicate", "Seçili kampanyanın aynı içerikli kopyası (içerik tekrarı demosu)."},
	{"ambiguous", "Router hangi kampanyanın kastedildiğini sorar."},
	{"refusal", "Model isteği yanıtlamaz."},
	{"max_tokens", "İlk yanıt uzunluk sınırında kesilir; döngü bir kez yüksek limitle yeniden dener."},
	{"save_then_llm_error", "Taslak kaydedilir, ardından model hatası: tur kısmi (partial) biter."},
	{"error_mid_stream", "Yanıt akarken bağlantı kopar; hiçbir şey kaydedilmez."},
	{"slow_stream", "Yavaş akış: bağlantı kopması ve kayıtlı sonucun okunması demosu."},
	{"english", "İçerik İngilizce istenmiş gibi: herkese aynı İngilizce taslak; yanıt Türkçe."},
	{"multilingual", "Herkese kendi dilinde: TR, EN ve FR varyantları ile ülke eşlemesi; karar bekleyen grupları sorar."},
	{"bilingual", "Herkese kendi dilinde ama yalnız TR ve FR varyantı: İngilizce okuyanlar varyant bekler."},
	{"group_decision", "Kullanıcının grup grup cevabı: en büyük bekleyen grup İngilizce, diğerleri gönderim dışı."},
	{"pending_to_english", "Kullanıcının genel cevabı: karar bekleyen herkes İngilizce; gerekirse İngilizce varyantı ekler."},
	{"ab_test", "A/B testi: taslağın içeriği A kalır, her içeriğe son günü öne çıkaran bir B eklenir."},
	{"remove_ab_test", "A/B testini kaldırma: model ayrı remove_ab_test çağrısını yapar; içerik ve kitle aynı kalır."},
}

var scenarioTag = regexp.MustCompile(`#scenario:([\p{L}0-9_]+)`)

// scenarioOf returns the scenario a message asks for, or "".
func scenarioOf(message string) string {
	if m := scenarioTag.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

func knownScenario(name string) bool {
	return slices.ContainsFunc(Scenarios, func(s Scenario) bool { return s.Name == name })
}

// Demo plays the model's part when the app runs without Claude, so the
// whole flow (routing, streamed drafts, repairs, failures) can be shown
// without an API key. It answers the router and the main model from the
// structured input they receive (routing_input, chat_state, tool results),
// never by understanding the user's prose; what it proposes goes through
// the same backend checks as Claude's answers. Its Info always reports provider
// "mock" with the reason, so the UI can say so.
type Demo struct {
	info llm.Info
	// SlowDelay is the chunk delay of the slow_stream scenario.
	SlowDelay time.Duration
}

// NewDemo returns the demo client; reason explains why mock is active.
func NewDemo(reason string) *Demo {
	return &Demo{info: llm.Info{Provider: "mock", Model: "mock-demo", Reason: reason}, SlowDelay: 150 * time.Millisecond}
}

// Info implements llm.Client.
func (d *Demo) Info() llm.Info { return d.info }

// Stream implements llm.Client.
func (d *Demo) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if slices.ContainsFunc(req.Tools, func(t llm.Tool) bool { return t.Name == routerTool }) {
		return Play(ctx, route(req), "toolu_demo_route"), nil
	}
	t := readTurn(req)
	c := t.script(req)
	if t.scenario == "slow_stream" {
		c.ChunkDelay = d.SlowDelay
	}
	return Play(ctx, c, fmt.Sprintf("toolu_demo_%d", t.rounds+1)), nil
}

// ---- router -------------------------------------------------------------------

type routingInput struct {
	SelectedTab *int64 `json:"selected_tab"`
	Campaigns   []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"campaigns"`
	Message string `json:"message"`
}

// route decides like a cautious router: the selected tab, else the only
// draft, else a question; scenarios force a new campaign, a copy or a
// question.
func route(req llm.Request) Call {
	var in routingInput
	_ = json.Unmarshal([]byte(between(lastUserText(req), "<routing_input>", "</routing_input>")), &in)
	drafts := []int64{}
	for _, c := range in.Campaigns {
		if c.Status == "draft" {
			drafts = append(drafts, c.ID)
		}
	}
	decide := func(intent, reason string, ids ...int64) Call {
		if ids == nil {
			ids = []int64{}
		}
		b, _ := json.Marshal(map[string]any{"intent": intent, "campaign_ids": ids, "reason": "mock: " + reason})
		return Call{ToolName: routerTool, ToolInput: string(b)}
	}
	switch scenarioOf(in.Message) {
	case "new_campaign":
		return decide("create", "#scenario:new_campaign")
	case "duplicate":
		switch {
		case in.SelectedTab != nil:
			return decide("create", "seçili sekmenin kopyası", *in.SelectedTab)
		case len(in.Campaigns) > 0:
			return decide("create", "ilk kampanyanın kopyası", in.Campaigns[0].ID)
		}
		return decide("create", "kopyalanacak kampanya yok")
	case "ambiguous":
		return decide("clarify", "#scenario:ambiguous", drafts...)
	}
	switch {
	case in.SelectedTab != nil:
		return decide("edit", "seçili sekme", *in.SelectedTab)
	case len(drafts) == 1:
		return decide("edit", "sohbetteki tek taslak", drafts...)
	case len(drafts) == 0:
		return decide("create", "düzenlenebilir taslak yok")
	}
	return decide("clarify", "birden çok taslak var, sekme seçili değil", drafts...)
}

// ---- main model -----------------------------------------------------------------

// chatState is the part of <chat_state> the demo reads.
type chatState struct {
	Campaigns []struct {
		ID int64 `json:"id"`
	} `json:"campaigns"`
	Target struct {
		AllowedCampaignIDs []int64 `json:"allowed_campaign_ids"`
		AllowCreate        bool    `json:"allow_create"`
	} `json:"target"`
	TargetCampaigns []targetCampaign `json:"target_campaigns"`
}

type targetCampaign struct {
	ID           int64              `json:"id"`
	Ref          string             `json:"ref"`
	Name         string             `json:"name"`
	ReadOnly     bool               `json:"read_only"`
	Version      int                `json:"version"`
	SegmentRule  json.RawMessage    `json:"segment_rule"`
	SMS          *smsText           `json:"sms"`
	Email        json.RawMessage    `json:"email"`
	Localization *language.Settings `json:"localization"`
	Languages    *language.Summary  `json:"languages"`
	ABTest       *abtest.Test       `json:"ab_test"`
}

func (c targetCampaign) perRecipient() bool {
	return c.Localization != nil && c.Localization.Mode == language.ModePerRecipient
}

type smsText struct {
	Text string `json:"text"`
}

// toolResult is what the demo reads of a tool_result.
type toolResult struct {
	rejected bool
	Saved    *struct {
		Ref     string `json:"ref"`
		Version int    `json:"version"`
		Created bool   `json:"created"`
	} `json:"saved"`
	Audience *struct {
		Total          int `json:"total"`
		SMSReachable   int `json:"sms_reachable"`
		EmailReachable int `json:"email_reachable"`
	} `json:"audience"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Languages     *language.Summary `json:"languages"`
	ABTestRemoved bool              `json:"ab_test_removed"`
}

// turn is what the demo sees of the current turn.
type turn struct {
	state    chatState
	message  string
	scenario string
	rounds   int          // answers already given in this turn
	last     []toolResult // tool results of the last round
	rejected int          // rejected drafts so far
}

func readTurn(req llm.Request) turn {
	var t turn
	start := -1
	for i := len(req.Messages) - 1; i >= 0 && start < 0; i-- {
		if m := req.Messages[i]; m.Role == llm.RoleUser && len(m.Blocks) > 0 && strings.HasPrefix(m.Blocks[0].Text, "<chat_state>") {
			start = i
		}
	}
	if start < 0 {
		return t
	}
	first := req.Messages[start]
	_ = json.Unmarshal([]byte(between(first.Blocks[0].Text, "<chat_state>", "</chat_state>")), &t.state)
	for _, b := range first.Blocks[1:] {
		t.message += b.Text
	}
	t.scenario = scenarioOf(t.message)
	for _, m := range req.Messages[start+1:] {
		if m.Role == llm.RoleAssistant {
			t.rounds++
			continue
		}
		t.last = nil
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolResult {
				continue
			}
			r := toolResult{rejected: b.IsError}
			_ = json.Unmarshal([]byte(b.Text), &r)
			if r.rejected {
				t.rejected++
			}
			t.last = append(t.last, r)
		}
	}
	return t
}

func (t turn) lastRejected() bool {
	return len(t.last) > 0 && t.last[len(t.last)-1].rejected
}

// script is the demo's answer for this point of the turn.
func (t turn) script(req llm.Request) Call {
	if t.scenario != "" && !knownScenario(t.scenario) {
		names := make([]string, len(Scenarios))
		for i, s := range Scenarios {
			names[i] = s.Name
		}
		return Call{Text: "Bilinmeyen mock senaryosu: " + t.scenario + ". Bilinen senaryolar: " + strings.Join(names, ", ") + "."}
	}
	if t.rounds > 0 {
		switch {
		case t.scenario == "save_then_llm_error":
			return Call{Err: llm.ErrUnavailable}
		case t.scenario == "repair" && t.lastRejected() && t.rejected == 1:
			return t.draft("SMS'i 160 karakter sınırına göre kısaltıp yeniden kaydediyorum. ")
		case t.scenario == "unknown_city" && t.lastRejected():
			return Call{Text: "\"Atlantis\" konum kataloğunda yok, bu yüzden taslağı kaydetmedim. " +
				"Hangi şehri kastettiniz? Örneğin İstanbul, Ankara ya da İzmir."}
		}
		return Call{Text: t.summary()}
	}
	switch t.scenario {
	case "refusal":
		return Call{Text: "Bu isteğe yardımcı olamam.", Stop: llm.StopRefusal}
	case "error_mid_stream":
		return Call{Text: "Taslağı hazırlıyorum: önce hedef kitleyi çıkarıyorum, ardından SMS metnini yazacağım.",
			Err: llm.ErrStreamInterrupted, ErrAfterEvents: 6}
	case "max_tokens":
		if req.MaxTokens < retryMaxTokens {
			c := t.draft("Taslağı hazırlıyorum. ")
			c.ToolInput = truncateRunes(c.ToolInput, utf8.RuneCountInString(c.ToolInput)/2)
			c.Stop = llm.StopMaxTokens
			return c
		}
	case "repair":
		c := t.draft("Taslağı hazırlıyorum. ")
		c.ToolInput = withSMS(c.ToolInput, truncateRunes(strings.Repeat("Sepetindeki ürünler seni bekliyor! ", 5), 172))
		return c
	case "unknown_city":
		c := t.draft("Taslağı hazırlıyorum. ")
		c.ToolInput = withRule(c.ToolInput, `{"match":"all","conditions":[{"kind":"location","operator":"in",`+
			`"places":[{"level":"city","name":"Atlantis"}]}]}`)
		return c
	case "english":
		return t.english()
	case "multilingual":
		return t.perRecipient("tr", "en", "fr")
	case "bilingual":
		return t.perRecipient("tr", "fr")
	case "group_decision", "pending_to_english":
		return t.decide(t.scenario == "pending_to_english")
	case "ab_test":
		return t.abTest()
	case "remove_ab_test":
		return t.removeAB()
	}
	return t.draft("")
}

// draft writes the turn's draft: a new campaign (or an exact copy of the
// referenced one) when the turn may create, else a change to its first
// writable target; a turn that may write nothing gets a plain answer.
func (t turn) draft(intro string) Call {
	if t.state.Target.AllowCreate {
		if src := t.reference(); src != nil && t.scenario == "duplicate" {
			in := map[string]any{"name": src.Name + " (kopya)", "segment_rule": src.SegmentRule,
				"change_summary": src.Ref + " ile aynı içerik"}
			if src.perRecipient() {
				// A new campaign: its groups are put to the user again.
				loc := *src.Localization
				loc.Decisions = nil
				in["per_recipient"] = perRecipientInput(loc)
			} else {
				in["content_language"] = contentLanguage(*src)
				if src.SMS != nil {
					in["sms"] = src.SMS
				}
				if present(src.Email) {
					in["email"] = src.Email
				}
			}
			if src.ABTest != nil {
				in["ab_test"] = src.ABTest
			}
			return Call{Text: intro + src.Ref + " (" + src.Name + ") ile aynı içerikte yeni bir kampanya hazırlıyorum.",
				ToolName: draftTool, ToolInput: mustJSON(in)}
		}
		s := samples[len(t.state.Campaigns)%len(samples)]
		in := map[string]any{"name": s.name, "segment_rule": json.RawMessage(s.rule), "sms": map[string]string{"text": s.sms},
			"email": map[string]string{"subject": s.subject, "body": s.body}, "content_language": "tr", "change_summary": "İlk taslak"}
		// The demo does not read the brief; it says so rather than pass the
		// next sample off as a reading of the request.
		text := intro + "Mock model, hazır demo taslağı ekliyor: " + s.name + " (isteğiniz yorumlanmadı)."
		return Call{Text: text, ToolName: draftTool, ToolInput: mustJSON(in)}
	}
	if c := t.writable(); c != nil {
		in := map[string]any{"campaign_id": c.ID, "base_version": c.Version, "name": c.Name, "segment_rule": c.SegmentRule}
		var summary string
		if c.perRecipient() {
			// The Turkish variant gets the change; the others stay as they are.
			loc := *c.Localization
			loc.Variants = slices.Clone(loc.Variants)
			for i, v := range loc.Variants {
				if v.Language == "tr" && v.SMS != nil {
					var text string
					text, summary = lastDay(&smsText{Text: v.SMS.Text})
					loc.Variants[i].SMS = &content.SMS{Text: text}
				}
			}
			if summary == "" {
				summary = "Değişiklik yok"
			}
			in["per_recipient"] = perRecipientInput(loc)
			carryB(in, c.ABTest, loc.Variants)
		} else {
			var text string
			text, summary = lastDay(c.SMS)
			in["sms"], in["content_language"] = map[string]string{"text": text}, contentLanguage(*c)
			if present(c.Email) {
				in["email"] = c.Email
			}
			carryB(in, c.ABTest, []language.Variant{{Language: contentLanguage(*c), SMS: &content.SMS{Text: text}, Email: emailOf(c.Email)}})
		}
		in["change_summary"] = summary
		return Call{Text: intro + c.Ref + " (" + c.Name + ") için yeni versiyon hazırlıyorum: " + summary + ".",
			ToolName: draftTool, ToolInput: mustJSON(in)}
	}
	return Call{Text: "Mock modunda bu mesaja kampanya yazmadan yanıt veriyorum. Bir kampanyayı değiştirmek için " +
		"sekmesini seçip isteğinizi yazın; yeni kampanya için mesaja #scenario:new_campaign ekleyin."}
}

// reference is the read-only campaign shown with a create (a copy's source).
func (t turn) reference() *targetCampaign {
	for i, c := range t.state.TargetCampaigns {
		if c.ReadOnly {
			return &t.state.TargetCampaigns[i]
		}
	}
	return nil
}

// writable is the first campaign the turn may write.
func (t turn) writable() *targetCampaign {
	for i, c := range t.state.TargetCampaigns {
		if !c.ReadOnly && slices.Contains(t.state.Target.AllowedCampaignIDs, c.ID) {
			return &t.state.TargetCampaigns[i]
		}
	}
	return nil
}

// summary reports what the last round saved or why it was not saved.
func (t turn) summary() string {
	var parts []string
	for _, r := range t.last {
		switch {
		case r.Saved != nil:
			s := fmt.Sprintf("%s v%d kaydedildi", r.Saved.Ref, r.Saved.Version)
			if r.Saved.Created {
				s += " (yeni kampanya)"
			}
			if r.ABTestRemoved {
				s += "; A/B testi kaldırıldı, herkes A metnini alacak"
			}
			if a := r.Audience; a != nil {
				s += fmt.Sprintf(". Kitle: %d kişi; SMS'e uygun %d, e-postaya uygun %d", a.Total, a.SMSReachable, a.EmailReachable)
			}
			parts = append(parts, s+".")
			parts = append(parts, languageLines(r.Languages)...)
		case r.rejected && len(r.Errors) > 0:
			parts = append(parts, "Taslak kaydedilemedi: "+r.Errors[0].Message+".")
		case r.rejected:
			parts = append(parts, "Taslak kaydedilemedi.")
		}
	}
	if len(parts) == 0 {
		return "Tamam."
	}
	return strings.Join(parts, "\n") + "\nOnaylamadan önce sağdaki panelden inceleyip düzenleyebilirsiniz."
}

// ---- languages ------------------------------------------------------------------

// english plays a brief that asks for English content: one English draft
// for everyone (a new campaign, or the target rewritten), the reply in the
// brief's language.
func (t turn) english() Call {
	en := cartVariants["en"]
	in := map[string]any{"content_language": "en", "sms": en.SMS, "email": en.Email}
	if c := t.writable(); c != nil && !t.state.Target.AllowCreate {
		in["campaign_id"], in["base_version"], in["name"], in["segment_rule"] = c.ID, c.Version, c.Name, c.SegmentRule
		in["change_summary"] = "İçerik İngilizceye çevrildi"
		carryB(in, c.ABTest, []language.Variant{{Language: "en", SMS: en.SMS, Email: en.Email}})
		return Call{Text: c.Ref + " (" + c.Name + ") içeriğini İngilizceye çeviriyorum; herkese aynı İngilizce metin gidecek.",
			ToolName: draftTool, ToolInput: mustJSON(in)}
	}
	if !t.state.Target.AllowCreate {
		return t.draft("")
	}
	in["name"], in["segment_rule"], in["change_summary"] = "Sepet hatırlatma (İngilizce)", json.RawMessage(cartRule), "İlk taslak"
	return Call{Text: "Mock model, hazır demo taslağının İngilizce halini ekliyor: herkese aynı İngilizce metin gidecek, " +
		"tercihe göre dağıtılmayacak (isteğiniz yorumlanmadı).", ToolName: draftTool, ToolInput: mustJSON(in)}
}

// perRecipient plays "send everyone their own language" with variants in
// langs: a new campaign, or the target with its rule kept. The country
// mapping covers Türkiye, France, the US and the UK; Canada (two languages)
// and Germany (no supported language) are left for the user.
func (t turn) perRecipient(langs ...string) Call {
	loc := language.Settings{Mode: language.ModePerRecipient, Countries: []language.CountryLanguage{
		{Country: "TR", Language: "tr"}, {Country: "FR", Language: "fr"}, {Country: "US", Language: "en"}, {Country: "GB", Language: "en"},
	}}
	for _, l := range langs {
		loc.Variants = append(loc.Variants, cartVariants[l])
	}
	names := make([]string, len(langs))
	for i, l := range langs {
		names[i] = languageName(l)
	}
	in := map[string]any{"per_recipient": perRecipientInput(loc)}
	if c := t.writable(); c != nil && !t.state.Target.AllowCreate {
		in["campaign_id"], in["base_version"], in["name"], in["segment_rule"] = c.ID, c.Version, c.Name, c.SegmentRule
		in["change_summary"] = "Her alıcıya kendi dilinde: " + strings.Join(names, ", ")
		carryB(in, c.ABTest, loc.Variants)
		return Call{Text: c.Ref + " (" + c.Name + ") her alıcıya kendi dilinde gidecek: " + strings.Join(names, ", ") + " varyantları.",
			ToolName: draftTool, ToolInput: mustJSON(in)}
	}
	if !t.state.Target.AllowCreate {
		return t.draft("")
	}
	in["name"], in["segment_rule"], in["change_summary"] = "Sepet hatırlatma · kendi dilinde", json.RawMessage(cartRule), "İlk taslak"
	return Call{Text: "Mock model, hazır demo taslağını her alıcıya kendi dilinde gidecek şekilde ekliyor: " +
		strings.Join(names, ", ") + " varyantları (isteğiniz yorumlanmadı).", ToolName: draftTool, ToolInput: mustJSON(in)}
}

// decide plays the user's answer about the waiting groups of the target:
// group by group (the largest group gets English, the others are left out)
// or, with everyone, every waiting group gets English and the English
// variant is added when it is missing.
func (t turn) decide(everyone bool) Call {
	c := t.writable()
	if c == nil || !c.perRecipient() || c.Languages == nil || len(c.Languages.Unresolved) == 0 {
		return Call{Text: "Bu kampanyada karar bekleyen dil grubu yok."}
	}
	loc := *c.Localization
	loc.Variants, loc.Decisions = slices.Clone(loc.Variants), slices.Clone(loc.Decisions)
	_, hasEnglish := loc.Variant("en")
	summary := "Karar bekleyen gruplar için karar verildi"
	if everyone {
		summary = "Karar bekleyen herkes İngilizce alacak"
		if !hasEnglish {
			en := cartVariants["en"]
			if loc.Variants[0].SMS == nil {
				en.SMS = nil
			}
			if loc.Variants[0].Email == nil {
				en.Email = nil
			}
			loc.Variants = append(loc.Variants, en)
			hasEnglish = true
			summary += "; İngilizce varyant eklendi"
		}
	}
	for i, g := range c.Languages.Unresolved { // largest first
		switch {
		case g.Group == "variant:en" && everyone:
			// the English variant now covers them
		case (everyone || i == 0) && hasEnglish:
			loc.Decisions = append(loc.Decisions, language.Decision{Group: g.Group, Action: language.ActionAssign, Language: "en"})
		default:
			loc.Decisions = append(loc.Decisions, language.Decision{Group: g.Group, Action: language.ActionExclude})
		}
	}
	in := map[string]any{"campaign_id": c.ID, "base_version": c.Version, "name": c.Name, "segment_rule": c.SegmentRule,
		"per_recipient": perRecipientInput(loc), "change_summary": summary}
	carryB(in, c.ABTest, loc.Variants)
	return Call{Text: c.Ref + " (" + c.Name + ") için yeni versiyon hazırlıyorum: " + summary + ".",
		ToolName: draftTool, ToolInput: mustJSON(in)}
}

// abTest plays "make it an A/B test": the content stays A and every A
// content gets a B that leads with the last day (a B the campaign already
// has, and that still fits, is kept); a new campaign is the demo's cart
// draft with such a B.
func (t turn) abTest() Call {
	if c := t.writable(); c != nil && !t.state.Target.AllowCreate {
		in := map[string]any{"campaign_id": c.ID, "base_version": c.Version, "name": c.Name, "segment_rule": c.SegmentRule,
			"change_summary": "A/B testi: B son günü öne çıkarıyor"}
		var a []language.Variant
		if c.perRecipient() {
			in["per_recipient"] = perRecipientInput(*c.Localization)
			a = c.Localization.Variants
		} else {
			var sms *content.SMS
			if c.SMS != nil {
				sms = &content.SMS{Text: c.SMS.Text}
			}
			in["content_language"], in["sms"], in["email"] = contentLanguage(*c), sms, emailOf(c.Email)
			a = []language.Variant{{Language: contentLanguage(*c), SMS: sms, Email: emailOf(c.Email)}}
		}
		prev := c.ABTest
		if prev == nil {
			prev = &abtest.Test{}
		}
		carryB(in, prev, a)
		return Call{Text: c.Ref + " (" + c.Name + ") için A/B testi hazırlıyorum: mevcut metin A, son günü öne çıkaran metin B. " +
			"Alıcıların yaklaşık yarısı A'yı, yarısı B'yi alacak; iki kanalda da aynı varyant.", ToolName: draftTool, ToolInput: mustJSON(in)}
	}
	if !t.state.Target.AllowCreate {
		return t.draft("")
	}
	tr := cartVariants["tr"]
	in := map[string]any{"name": "Sepet hatırlatma · A/B", "segment_rule": json.RawMessage(cartRule), "change_summary": "İlk taslak",
		"content_language": "tr", "sms": tr.SMS, "email": tr.Email}
	carryB(in, &abtest.Test{}, []language.Variant{tr})
	return Call{Text: "Mock model, hazır demo taslağını A/B testiyle ekliyor: A hazır metin, B son günü öne çıkarıyor " +
		"(isteğiniz yorumlanmadı).", ToolName: draftTool, ToolInput: mustJSON(in)}
}

// removeAB plays "remove the A/B test": the separate removal call on the
// target, which leaves its content and audience as they are.
func (t turn) removeAB() Call {
	c := t.writable()
	switch {
	case c == nil:
		return Call{Text: "A/B testini kaldırmak için kampanyanın sekmesini seçip isteğinizi yeniden yazın."}
	case c.ABTest == nil:
		return Call{Text: c.Ref + " (" + c.Name + ") için kaldırılacak bir A/B testi yok."}
	}
	in := map[string]any{"campaign_id": c.ID, "base_version": c.Version, "change_summary": "A/B testi kaldırıldı"}
	return Call{Text: c.Ref + " (" + c.Name + ") için A/B testini kaldırıyorum; içerik ve kitle aynı kalacak.",
		ToolName: removeABTool, ToolInput: mustJSON(in)}
}

// carryB puts an A/B test on a draft whose A contents are a, when the
// campaign has one (prev): each A content keeps its B when that B still
// fits it, and gets a new one otherwise.
func carryB(in map[string]any, prev *abtest.Test, a []language.Variant) {
	if prev == nil {
		return
	}
	out := make([]language.Variant, 0, len(a))
	for _, av := range a {
		if b, ok := prev.Variant(av.Language); ok &&
			len(abtest.Validate(abtest.Test{Variants: []language.Variant{b}}, []language.Variant{av})) == 0 {
			out = append(out, b)
			continue
		}
		out = append(out, bVariant(av))
	}
	in["ab_test"] = map[string]any{"variants": out}
}

// The demo's B tests a last-day call: its lead goes before A's text; an
// SMS the lead does not fit before gets a short last-day text instead.
var (
	lastDayLead = map[string]string{"tr": "Son gün! ", "en": "Last day! ", "fr": "Dernier jour ! "}
	lastDaySMS  = map[string]string{
		"tr": "Son gün {{first_name}}! Sepetindeki ürünler seni bekliyor.",
		"en": "Last day {{first_name}}! Your cart is waiting.",
		"fr": "Dernier jour {{first_name}} ! Votre panier vous attend.",
	}
)

const subjectLimit = 80

func bVariant(a language.Variant) language.Variant {
	lang := a.Language
	if _, ok := lastDayLead[lang]; !ok {
		lang = "tr"
	}
	b := language.Variant{Language: a.Language}
	if a.SMS != nil {
		text := lastDayLead[lang] + a.SMS.Text
		if estimatedRunes(text) > smsLimit {
			text = lastDaySMS[lang]
		}
		b.SMS = &content.SMS{Text: text}
	}
	if a.Email != nil {
		subject := lastDayLead[lang] + a.Email.Subject
		if estimatedRunes(subject) > subjectLimit {
			subject = strings.TrimSpace(lastDayLead[lang])
		}
		b.Email = &content.Email{Subject: subject, Body: a.Email.Body}
	}
	return b
}

// emailOf reads a campaign's email from chat_state (nil: none).
func emailOf(raw json.RawMessage) *content.Email {
	if !present(raw) {
		return nil
	}
	var e content.Email
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil
	}
	return &e
}

// perRecipientInput is the per_recipient part of a draft.
func perRecipientInput(loc language.Settings) map[string]any {
	in := map[string]any{"variants": loc.Variants}
	if len(loc.Countries) > 0 {
		in["country_languages"] = loc.Countries
	}
	if len(loc.Decisions) > 0 {
		in["decisions"] = loc.Decisions
	}
	return in
}

// contentLanguage is the language of a campaign's one content (Turkish for
// a campaign from before there were languages).
func contentLanguage(c targetCampaign) string {
	if c.Localization != nil && c.Localization.Language != "" {
		return c.Localization.Language
	}
	return "tr"
}

// languageLines reports how a saved draft's recipients split by language,
// as of the draft, and asks about the groups that wait for a decision.
func languageLines(l *language.Summary) []string {
	if l == nil {
		return nil
	}
	var lines []string
	if l.Mode == language.ModePerRecipient {
		var parts []string
		for _, code := range language.Supported {
			if n := l.Languages[code].Total; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", languageName(code), n))
			}
		}
		if len(parts) > 0 {
			lines = append(lines, "Taslak anında mesaj alabilecek alıcılar: "+strings.Join(parts, ", ")+".")
		}
	} else if l.Mode == language.ModeSingle {
		for code := range l.Languages {
			lines = append(lines, "Herkese aynı "+languageName(code)+" metin gidecek; tercihe göre dağıtılmayacak.")
		}
	}
	if len(l.Unresolved) > 0 {
		groups := make([]string, len(l.Unresolved))
		for i, g := range l.Unresolved {
			groups[i] = fmt.Sprintf("%s: %d kişi", groupLabel(g.Group), g.Count)
		}
		names := make([]string, len(language.Supported))
		for i, code := range language.Supported {
			names[i] = languageName(code)
		}
		lines = append(lines, "Karar bekleyen alıcılar var: "+strings.Join(groups, "; ")+".",
			"Bu gruplar desteklenen dillerden birine mi ("+strings.Join(names, ", ")+") atansın, "+
				"yoksa gönderimden mi çıkarılsın? Karar verilene kadar onay kilitli.")
	}
	return lines
}

// groupLabel names a waiting group the way the demo asks about it: by the
// names of the language and the country, never by their codes.
func groupLabel(group string) string {
	kind, value, _ := strings.Cut(group, ":")
	switch kind {
	case language.KindPreference:
		return "tercihi " + languageName(value) + ", desteklenmiyor"
	case language.KindCountry:
		name := value
		if c, ok := geo.Default().Country(value); ok {
			name = c.Name
		}
		return name + ", kayıtlı tercihi yok"
	case language.KindVariant:
		return languageName(value) + " okuyanlar, bu dilde varyant yok"
	}
	return group
}

var languageNames = map[string]string{
	"tr": "Türkçe", "en": "İngilizce", "fr": "Fransızca", "de": "Almanca", "ar": "Arapça",
	"es": "İspanyolca", "it": "İtalyanca", "ru": "Rusça", "pt": "Portekizce",
}

func languageName(code string) string {
	if n, ok := languageNames[code]; ok {
		return n
	}
	return code
}

// cartRule is everyone who left a cart in the last 30 days, wherever they
// live: the multilingual demo's audience.
const cartRule = `{"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]}`

// cartVariants are the multilingual demo's content, one per language.
var cartVariants = map[string]language.Variant{
	"tr": {Language: "tr",
		SMS: &content.SMS{Text: "Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! Siparişini bugün tamamlarsan kargo bizden."},
		Email: &content.Email{Subject: "{{first_name}}, sepetin seni bekliyor",
			Body: "Merhaba {{first_name}},\n\nSepetine eklediğin ürünler hâlâ seni bekliyor. Siparişini bugün tamamlarsan kargo bizden.\n\nİyi alışverişler!"}},
	"en": {Language: "en",
		SMS: &content.SMS{Text: "Hi {{first_name}}, the items in your cart are waiting for you! Order today and shipping is on us."},
		Email: &content.Email{Subject: "{{first_name}}, your cart is waiting",
			Body: "Hi {{first_name}},\n\nThe items you added to your cart are still waiting for you. Order today and shipping is on us.\n\nHappy shopping!"}},
	"fr": {Language: "fr",
		SMS: &content.SMS{Text: "Bonjour {{first_name}}, les articles de votre panier vous attendent ! Commandez aujourd'hui, la livraison est offerte."},
		Email: &content.Email{Subject: "{{first_name}}, votre panier vous attend",
			Body: "Bonjour {{first_name}},\n\nLes articles ajoutés à votre panier vous attendent toujours. Commandez aujourd'hui, la livraison est offerte.\n\nBonnes emplettes !"}},
}

// ---- content --------------------------------------------------------------------

type sample struct{ name, rule, sms, subject, body string }

// samples are the demo's new campaigns, picked in turn.
var samples = []sample{
	{
		name: "İstanbul sepet hatırlatma",
		rule: `{"match":"all","conditions":[{"kind":"location","operator":"in","places":[{"level":"city","name":"İstanbul","country":"TR"}]},` +
			`{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]}`,
		sms:     "Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! Siparişini bugün tamamlarsan kargo bizden.",
		subject: "{{first_name}}, sepetin seni bekliyor",
		body: "Merhaba {{first_name}},\n\nSepetine eklediğin ürünler hâlâ seni bekliyor. Siparişini bugün tamamlarsan " +
			"kargo bizden.\n\nİyi alışverişler!",
	},
	{
		name: "Ankara hoş geldin",
		rule: `{"match":"all","conditions":[{"kind":"location","operator":"in","places":[{"level":"city","name":"Ankara","country":"TR"}]},` +
			`{"kind":"attribute","field":"signup_days_ago","operator":"lte","values":["30"]}]}`,
		sms:     "Hoş geldin {{first_name}}! İlk siparişine özel %15 indirim kodun: HOSGELDIN15",
		subject: "Aramıza hoş geldin, {{first_name}}",
		body:    "Merhaba {{first_name}},\n\nAramıza hoş geldin! İlk siparişinde HOSGELDIN15 koduyla %15 indirim seni bekliyor.",
	},
	{
		name: "İzmir sadık müşteri",
		rule: `{"match":"all","conditions":[{"kind":"location","operator":"in","places":[{"level":"city","name":"İzmir","country":"TR"}]},` +
			`{"kind":"attribute","field":"total_orders","operator":"gte","values":["3"]}]}`,
		sms:     "{{first_name}}, bizi tercih ettiğin için teşekkürler! Sana özel %10 indirim kodun: SADIK10",
		subject: "Teşekkürler {{first_name}}, sana özel bir indirim var",
		body:    "Merhaba {{first_name}},\n\nBizi tercih ettiğin için teşekkür ederiz. SADIK10 koduyla bir sonraki siparişinde %10 indirim kazan.",
	},
}

const lastDaySuffix = " Son gün bugün!"

// lastDay toggles a closing "last day" line on an SMS, keeping the rest of
// its text (a manual edit included) as it is.
func lastDay(sms *smsText) (string, string) {
	switch {
	case sms == nil:
		return "Merhaba {{first_name}}, kampanyamız devam ediyor." + lastDaySuffix, "SMS eklendi"
	case strings.HasSuffix(sms.Text, lastDaySuffix):
		return strings.TrimSuffix(sms.Text, lastDaySuffix), "SMS'teki son gün vurgusu kaldırıldı"
	case estimatedRunes(sms.Text+lastDaySuffix) <= smsLimit:
		return sms.Text + lastDaySuffix, "SMS'e son gün vurgusu eklendi"
	}
	return sms.Text, "SMS sınıra yakın olduğu için metne dokunulmadı"
}

// smsLimit and variableEstimate mirror the validator's draft check
// (content.ValidateDraft): a variable counts as 10 characters.
const (
	smsLimit         = 160
	variableEstimate = 10
)

var placeholder = regexp.MustCompile(`\{\{\s*\w+\s*\}\}`)

func estimatedRunes(text string) int {
	return utf8.RuneCountInString(placeholder.ReplaceAllString(text, strings.Repeat("x", variableEstimate)))
}

func withSMS(input, text string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	m["sms"] = map[string]string{"text": text}
	return mustJSON(m)
}

func withRule(input, rule string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	m["segment_rule"] = json.RawMessage(rule)
	return mustJSON(m)
}

// ---- helpers --------------------------------------------------------------------

func lastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if m := req.Messages[i]; m.Role == llm.RoleUser && len(m.Blocks) > 0 {
			return m.Blocks[0].Text
		}
	}
	return ""
}

func between(s, open, close string) string {
	_, rest, ok := strings.Cut(s, open)
	if !ok {
		return ""
	}
	inner, _, _ := strings.Cut(rest, close)
	return inner
}

func present(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func truncateRunes(s string, n int) string {
	if n >= utf8.RuneCountInString(s) {
		return s
	}
	return string([]rune(s)[:n])
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
