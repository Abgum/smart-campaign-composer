package campaign

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/abtest"
)

// DB is what the store works on: a pool, or a transaction, in which the
// store's own transactions become savepoints (the demo history loads
// campaigns that way, all at once).
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store persists campaigns and versions.
type Store struct {
	pool DB
}

// NewStore returns a store on pool.
func NewStore(pool DB) *Store { return &Store{pool: pool} }

// Author identifies who writes a version: the chat, the producing turn
// (empty for manual edits) and the source ("llm" | "user").
type Author struct {
	ChatID   string
	TurnID   string
	Source   string
	Provider string
	Model    string
}

// DraftWriter writes drafts for one author.
type DraftWriter struct {
	s *Store
	a Author
}

// Drafts binds the store to an author.
func (s *Store) Drafts(a Author) *DraftWriter { return &DraftWriter{s: s, a: a} }

// SaveDraft writes a version in its own transaction. Creating a campaign
// locks the chat row so concurrent creations get distinct ordinals; revising
// locks the campaign row, which also serializes against approval.
//
// A revision that sends no A/B test keeps the base version's, unless
// DropABTest: a draft that leaves the test out never removes it. The kept B
// side is checked against the revision's A contents under the same lock;
// when it no longer fits, nothing is written (OutdatedABTestError).
func (w *DraftWriter) SaveDraft(ctx context.Context, cmd DraftCmd) (Saved, error) {
	tx, err := w.s.pool.Begin(ctx)
	if err != nil {
		return Saved{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	saved := Saved{ChangeSummary: cmd.ChangeSummary}
	if cmd.CampaignID == nil {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM chats WHERE id = $1 FOR UPDATE`, w.a.ChatID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Saved{}, ErrNotFound
			}
			return Saved{}, err
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO campaigns (chat_id, ordinal, name, status)
			VALUES ($1, (SELECT coalesce(max(ordinal), 0) + 1 FROM campaigns WHERE chat_id = $1), $2, 'draft')
			RETURNING id, ordinal`, w.a.ChatID, cmd.Name).Scan(&saved.CampaignID, &saved.Ordinal)
		if err != nil {
			return Saved{}, fmt.Errorf("insert campaign: %w", err)
		}
		saved.Version, saved.Created = 1, true
	} else {
		b, err := w.lockBase(ctx, tx, *cmd.CampaignID, cmd.BaseVersion)
		if err != nil {
			return Saved{}, err
		}
		saved.CampaignID, saved.Ordinal, saved.Version = *cmd.CampaignID, b.ordinal, b.version+1
		if cmd.ABTest == nil && !cmd.DropABTest && b.ab != nil {
			if issues := checkABTest(b.ab, abtest.Contents(cmd.Localization, cmd.SMS, cmd.Email)); len(issues) > 0 {
				return Saved{}, &OutdatedABTestError{Issues: issues}
			}
			cmd.ABTest, saved.ABTestKept = b.ab, true
		}
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO campaign_versions (campaign_id, version, source, llm_provider, llm_model, name,
		                               segment_rule, sms, email, audience, change_summary, created_by_turn_id,
		                               localization, language_summary, ab_test)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`,
		saved.CampaignID, saved.Version, w.a.Source, nullable(w.a.Provider), nullable(w.a.Model), cmd.Name,
		cmd.Rule, cmd.SMS, cmd.Email, cmd.Audience, cmd.ChangeSummary, nullable(w.a.TurnID),
		cmd.Localization, cmd.Languages, cmd.ABTest,
	).Scan(&saved.VersionID)
	if err != nil {
		return Saved{}, fmt.Errorf("insert version: %w", err)
	}
	if err := w.finish(ctx, tx, saved, cmd.Name); err != nil {
		return Saved{}, err
	}
	return saved, nil
}

// RemoveABTest writes a new version of a draft without its A/B test: the
// base version as it is, but for the B side. A draft saved without ab_test
// keeps the test, so this is the one way a turn removes it, for when the
// user asks; ErrNoABTest when the base version has none.
func (w *DraftWriter) RemoveABTest(ctx context.Context, campaignID int64, baseVersion int, changeSummary string) (Removed, error) {
	tx, err := w.s.pool.Begin(ctx)
	if err != nil {
		return Removed{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	b, err := w.lockBase(ctx, tx, campaignID, &baseVersion)
	if err != nil {
		return Removed{}, err
	}
	if b.ab == nil {
		return Removed{}, ErrNoABTest
	}
	r := Removed{Saved: Saved{CampaignID: campaignID, Ordinal: b.ordinal, Version: b.version + 1, ChangeSummary: changeSummary}}
	var name string
	err = tx.QueryRow(ctx, `
		INSERT INTO campaign_versions (campaign_id, version, source, llm_provider, llm_model, name,
		                               segment_rule, sms, email, audience, change_summary, created_by_turn_id,
		                               localization, language_summary)
		SELECT campaign_id, $2, $3, $4, $5, name, segment_rule, sms, email, audience, $6, $7, localization, language_summary
		FROM campaign_versions WHERE id = $1
		RETURNING id, name, audience, language_summary`,
		*b.id, r.Version, w.a.Source, nullable(w.a.Provider), nullable(w.a.Model), changeSummary, nullable(w.a.TurnID),
	).Scan(&r.VersionID, &name, &r.Audience, &r.Languages)
	if err != nil {
		return Removed{}, fmt.Errorf("insert version: %w", err)
	}
	if err := w.finish(ctx, tx, r.Saved, name); err != nil {
		return Removed{}, err
	}
	return r, nil
}

// lockedDraft is a locked draft's current version.
type lockedDraft struct {
	ordinal int
	id      *int64 // nil: the campaign has no version yet
	version int
	ab      *abtest.Test
}

// lockBase locks a draft of the writer's chat and checks that version is
// its current one (StaleError otherwise).
func (w *DraftWriter) lockBase(ctx context.Context, tx pgx.Tx, campaignID int64, version *int) (lockedDraft, error) {
	var (
		b      lockedDraft
		status string
	)
	// Lock the campaign row alone. A join here would be re-checked after
	// the lock wait against the statement's old snapshot and miss a version
	// committed meanwhile; the version is read below with a new snapshot.
	err := tx.QueryRow(ctx, `
		SELECT status, ordinal, current_version_id FROM campaigns
		WHERE id = $1 AND chat_id = $2 FOR UPDATE`, campaignID, w.a.ChatID).Scan(&status, &b.ordinal, &b.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedDraft{}, ErrNotFound
	}
	if err != nil {
		return lockedDraft{}, err
	}
	if status != "draft" {
		return lockedDraft{}, ErrNotDraft
	}
	if b.id != nil {
		if err := tx.QueryRow(ctx, `SELECT version, ab_test FROM campaign_versions WHERE id = $1`, *b.id).Scan(&b.version, &b.ab); err != nil {
			return lockedDraft{}, err
		}
	}
	if version == nil || *version != b.version {
		return lockedDraft{}, &StaleError{CurrentVersion: b.version}
	}
	return b, nil
}

// finish makes a written version the campaign's current one, links it to
// the writing turn and commits.
func (w *DraftWriter) finish(ctx context.Context, tx pgx.Tx, saved Saved, name string) error {
	if _, err := tx.Exec(ctx, `UPDATE campaigns SET current_version_id = $2, name = $3, updated_at = now() WHERE id = $1`,
		saved.CampaignID, saved.VersionID, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE chats SET updated_at = now() WHERE id = $1`, w.a.ChatID); err != nil {
		return err
	}
	if w.a.TurnID != "" {
		action := "revised"
		if saved.Created {
			action = "created"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO turn_campaign_refs (turn_id, campaign_id, version_id, action) VALUES ($1, $2, $3, $4)`,
			w.a.TurnID, saved.CampaignID, saved.VersionID, action); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
