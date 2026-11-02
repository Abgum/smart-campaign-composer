package campaign_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

func scalar[T any](t *testing.T, db *testutil.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.Pool.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func newChatTurn(t *testing.T, db *testutil.DB) (chatID, turnID string) {
	t.Helper()
	chatID = scalar[string](t, db, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`)
	turnID = scalar[string](t, db, `INSERT INTO turns (chat_id, client_turn_id, request_hash, status)
		VALUES ($1, gen_random_uuid(), 'h', 'running') RETURNING id::text`, chatID)
	return chatID, turnID
}

func cmd(name string, campaignID *int64, base *int) campaign.DraftCmd {
	return campaign.DraftCmd{
		CampaignID: campaignID, BaseVersion: base, Name: name,
		Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{
			{Kind: "event", EventType: "cart_add", WithinDays: 30, MinCount: 1, NotFollowedBy: "purchase"}}},
		SMS:           &content.SMS{Text: "Merhaba {{first_name}}"},
		Audience:      segment.Audience{Total: 312, SMSReachable: 220, EmailReachable: 250},
		ChangeSummary: "özet " + name,
	}
}

func ptr[T any](v T) *T { return &v }

func TestSaveDraft_CreatesWithNextOrdinal(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID, turnID := newChatTurn(t, db)
	w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm", Provider: "mock", Model: "mock-scripted"})

	a, err := w.SaveDraft(ctx, cmd("A", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.SaveDraft(ctx, cmd("B", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if a.Ordinal != 1 || b.Ordinal != 2 || a.Version != 1 || !a.Created || a.VersionID == 0 {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if got := scalar[int64](t, db, `SELECT current_version_id FROM campaigns WHERE id=$1`, a.CampaignID); got != a.VersionID {
		t.Errorf("current_version_id = %d, want %d", got, a.VersionID)
	}
	if got := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, a.CampaignID); got != "draft" {
		t.Errorf("status = %s", got)
	}
	row := db.Pool.QueryRow(ctx, `SELECT version, source, llm_provider, llm_model, name,
		segment_rule->'conditions'->0->>'not_followed_by', sms->>'text', (audience->>'total')::int, change_summary,
		created_by_turn_id::text
		FROM campaign_versions WHERE id=$1`, a.VersionID)
	var (
		version, total                                       int
		source, provider, model, name, nfb, sms, sum, byTurn string
	)
	if err := row.Scan(&version, &source, &provider, &model, &name, &nfb, &sms, &total, &sum, &byTurn); err != nil {
		t.Fatal(err)
	}
	if version != 1 || source != "llm" || provider != "mock" || model != "mock-scripted" || name != "A" ||
		nfb != "purchase" || sms != "Merhaba {{first_name}}" || total != 312 || sum != "özet A" || byTurn != turnID {
		t.Errorf("version row = %d %s %s %s %s %s %q %d %q %s", version, source, provider, model, name, nfb, sms, total, sum, byTurn)
	}
	if got := scalar[string](t, db, `SELECT action FROM turn_campaign_refs WHERE turn_id=$1 AND version_id=$2`, turnID, a.VersionID); got != "created" {
		t.Errorf("ref action = %s", got)
	}
}

func TestSaveDraft_Revise(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID, turnID := newChatTurn(t, db)
	w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm", Provider: "mock"})
	a, _ := w.SaveDraft(ctx, cmd("A", nil, nil))

	r, err := w.SaveDraft(ctx, cmd("A yeni", &a.CampaignID, ptr(1)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 2 || r.Created || r.Ordinal != 1 || r.CampaignID != a.CampaignID {
		t.Fatalf("revised = %+v", r)
	}
	if got := scalar[int64](t, db, `SELECT current_version_id FROM campaigns WHERE id=$1`, a.CampaignID); got != r.VersionID {
		t.Errorf("current_version_id not moved")
	}
	if got := scalar[string](t, db, `SELECT name FROM campaigns WHERE id=$1`, a.CampaignID); got != "A yeni" {
		t.Errorf("campaign name = %q", got)
	}
	if got := scalar[string](t, db, `SELECT action FROM turn_campaign_refs WHERE version_id=$1`, r.VersionID); got != "revised" {
		t.Errorf("ref action = %s", got)
	}
}

func TestSaveDraft_StaleBaseVersion(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID, turnID := newChatTurn(t, db)
	w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm"})
	a, _ := w.SaveDraft(ctx, cmd("A", nil, nil))
	if _, err := w.SaveDraft(ctx, cmd("A2", &a.CampaignID, ptr(1))); err != nil {
		t.Fatal(err)
	}

	_, err := w.SaveDraft(ctx, cmd("A3", &a.CampaignID, ptr(1)))
	var stale *campaign.StaleError
	if !errors.As(err, &stale) || stale.CurrentVersion != 2 {
		t.Fatalf("err = %v, want stale with current 2", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM campaign_versions WHERE campaign_id=$1`, a.CampaignID); n != 2 {
		t.Fatalf("versions = %d; a stale write must not insert", n)
	}
	if _, err := w.SaveDraft(ctx, cmd("A3", &a.CampaignID, nil)); !errors.As(err, &stale) {
		t.Fatalf("missing base version must be treated as stale, got %v", err)
	}
}

func TestSaveDraft_NotDraftAndOtherChat(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID, turnID := newChatTurn(t, db)
	store := campaign.NewStore(db.Pool)
	w := store.Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm"})
	a, _ := w.SaveDraft(ctx, cmd("A", nil, nil))

	otherChat, otherTurn := newChatTurn(t, db)
	other := store.Drafts(campaign.Author{ChatID: otherChat, TurnID: otherTurn, Source: "llm"})
	if _, err := other.SaveDraft(ctx, cmd("x", &a.CampaignID, ptr(1))); !errors.Is(err, campaign.ErrNotFound) {
		t.Fatalf("other chat: err = %v, want ErrNotFound", err)
	}

	if _, err := db.Pool.Exec(ctx, `UPDATE campaigns SET status='approved' WHERE id=$1`, a.CampaignID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SaveDraft(ctx, cmd("x", &a.CampaignID, ptr(1))); !errors.Is(err, campaign.ErrNotDraft) {
		t.Fatalf("approved: err = %v, want ErrNotDraft", err)
	}
}

func TestSaveDraft_ConcurrentCreates_DistinctOrdinals(t *testing.T) {
	db := testutil.NewDB(t)
	chatID, turnID := newChatTurn(t, db)
	w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm"})

	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ordinals []int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := w.SaveDraft(context.Background(), cmd("c", nil, nil))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			ordinals = append(ordinals, s.Ordinal)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.Sort(ordinals)
	if !slices.Equal(ordinals, []int{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("ordinals = %v", ordinals)
	}
}

func TestSaveDraft_UserEditWithoutTurn(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chatID, turnID := newChatTurn(t, db)
	store := campaign.NewStore(db.Pool)
	a, _ := store.Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm"}).SaveDraft(ctx, cmd("A", nil, nil))

	u, err := store.Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, cmd("A", &a.CampaignID, ptr(1)))
	if err != nil {
		t.Fatal(err)
	}
	var source string
	var byTurn *string
	if err := db.Pool.QueryRow(ctx, `SELECT source, created_by_turn_id::text FROM campaign_versions WHERE id=$1`, u.VersionID).Scan(&source, &byTurn); err != nil {
		t.Fatal(err)
	}
	if source != "user" || byTurn != nil {
		t.Fatalf("user version: source=%s turn=%v", source, byTurn)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM turn_campaign_refs WHERE version_id=$1`, u.VersionID); n != 0 {
		t.Fatalf("a manual edit is not produced by a turn; refs = %d", n)
	}
}

// Two revisions of the same base race: one wins, the other is told the real
// current version (not 0, which a join evaluated against a stale snapshot
// after the row lock would report).
func TestSaveDraft_ConcurrentRevisions_StaleReportsCurrent(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		chatID, turnID := newChatTurn(t, db)
		w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm"})
		a, err := w.SaveDraft(ctx, cmd("A", nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		var (
			wg   sync.WaitGroup
			errs [2]error
		)
		for k := 0; k < 2; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[k] = w.SaveDraft(ctx, cmd("A", &a.CampaignID, ptr(1)))
			}()
		}
		wg.Wait()
		var stale *campaign.StaleError
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.As(err, &stale):
				if stale.CurrentVersion != 2 {
					t.Fatalf("round %d: loser told current version %d, want 2", i, stale.CurrentVersion)
				}
			default:
				t.Fatalf("round %d: unexpected error %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d revisions succeeded, want exactly 1", i, ok)
		}
	}
}
