// Package campaign owns campaigns, their versions and the approval flow.
package campaign

import (
	"errors"
	"fmt"
	"strings"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
)

// DraftCmd writes a new version of a campaign (or creates the campaign when
// CampaignID is nil). Rule must already be validated and normalized.
type DraftCmd struct {
	CampaignID    *int64
	BaseVersion   *int
	Name          string
	Rule          segment.Rule
	SMS           *content.SMS
	Email         *content.Email
	Audience      segment.Audience
	ChangeSummary string
	// Localization is the language setup (nil for a version written before
	// there was one); Languages is how the audience split by language.
	Localization *language.Settings
	Languages    *language.Summary
	// ABTest is the B side of an A/B test; nil for none. On a revision a nil
	// one keeps the base version's (see SaveDraft), unless DropABTest.
	ABTest *abtest.Test
	// DropABTest removes the base version's A/B test from a revision. Only
	// the marketer's own edit sets it (the editor's A/B switch turned off).
	DropABTest bool
}

// Saved describes a written version.
type Saved struct {
	CampaignID    int64  `json:"campaign_id"`
	Ordinal       int    `json:"ordinal"`
	Version       int    `json:"version"`
	VersionID     int64  `json:"version_id"`
	Created       bool   `json:"created"`
	ChangeSummary string `json:"change_summary"`
	// ABTestKept tells that the revision sent no A/B test and the base
	// version's was kept.
	ABTestKept bool `json:"-"`
}

// Removed is the version RemoveABTest wrote, with the audience and the
// language split it keeps from its base.
type Removed struct {
	Saved
	Audience  segment.Audience
	Languages *language.Summary
}

// Errors returned when a draft cannot be written.
var (
	ErrNotFound       = errors.New("campaign not found in this chat")
	ErrNotDraft       = errors.New("campaign is not a draft")
	ErrNotCancellable = errors.New("scheduled campaign can no longer be cancelled")
	ErrNoABTest       = errors.New("campaign draft has no A/B test")
)

// OutdatedABTestError means a revision sent no A/B test, so the base
// version's was kept, and its B side does not fit the revision's A contents.
type OutdatedABTestError struct {
	Issues []Issue
}

func (e *OutdatedABTestError) Error() string {
	msgs := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		msgs[i] = is.Path + ": " + is.Code
	}
	return "kept A/B test does not fit: " + strings.Join(msgs, ", ")
}

// StaleError means the caller edited an outdated version.
type StaleError struct {
	CurrentVersion int
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("stale base version; current version is %d", e.CurrentVersion)
}
