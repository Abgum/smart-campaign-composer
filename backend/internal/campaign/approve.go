package campaign

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
)

// ChannelCounts reports an approval snapshot for one channel.
type ChannelCounts struct {
	Selected          int `json:"selected"`
	ExcludedConsent   int `json:"excluded_consent"`
	ExcludedDuplicate int `json:"excluded_duplicate"`
}

// ApproveResult is the outcome of an approval. Languages splits the counts
// by the language of the variant each recipient got, and ExcludedLanguage
// counts who a language decision left out; both are empty for a version
// written before there were languages.
type ApproveResult struct {
	CampaignID       int64                               `json:"campaign_id"`
	VersionID        int64                               `json:"version_id"`
	Status           string                              `json:"status"`
	AlreadyApproved  bool                                `json:"already_approved"`
	Channels         map[string]ChannelCounts            `json:"channels"`
	Languages        map[string]map[string]ChannelCounts `json:"languages,omitempty"`
	ExcludedLanguage []language.GroupCount               `json:"excluded_language,omitempty"`
	ScheduledAt      *time.Time                          `json:"scheduled_at,omitempty"`
	// Variants splits the counts by A/B arm, then channel; empty without an
	// A/B test.
	Variants map[string]map[string]ChannelCounts `json:"variants,omitempty"`
}

// MaxScheduleAhead bounds how far ahead a campaign can be scheduled.
const MaxScheduleAhead = 365 * 24 * time.Hour

// ApproveOptions are what the marketer chooses when approving.
type ApproveOptions struct {
	// SendAt schedules the sending for a later moment; nil sends now. It is
	// kept in UTC and must lie after now, at most MaxScheduleAhead ahead.
	SendAt *time.Time
}

// checkSchedule tells why at cannot be a campaign's sending time, if it
// cannot.
func checkSchedule(at, now time.Time) []Issue {
	switch {
	case !at.After(now):
		return []Issue{{Path: "scheduled_at", Code: "schedule_in_past", Message: "gönderim zamanı gelecekte olmalı"}}
	case at.After(now.Add(MaxScheduleAhead)):
		return []Issue{{Path: "scheduled_at", Code: "schedule_too_far", Message: "gönderim zamanı en fazla bir yıl sonrası olabilir"}}
	}
	return nil
}

// approvalLanguages is the campaigns.approval_languages snapshot.
type approvalLanguages struct {
	Languages        map[string]map[string]ChannelCounts `json:"languages"`
	ExcludedLanguage []language.GroupCount               `json:"excluded_language,omitempty"`
}

// DecisionError means recipients wait for a language decision. Nothing is
// queued: sending starts only once every group is assigned to a variant or
// left out (in a new version) and that version is approved.
type DecisionError struct{ Groups []language.GroupCount }

func (e *DecisionError) Error() string {
	gs := make([]string, len(e.Groups))
	for i, g := range e.Groups {
		gs[i] = fmt.Sprintf("%s (%d)", g.Group, g.Count)
	}
	return "language decision required: " + strings.Join(gs, ", ")
}

// Issue is a validation finding at approval or edit time.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError means the version cannot be approved or saved as is.
type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		msgs[i] = is.Path + ": " + is.Code
	}
	return "validation failed: " + strings.Join(msgs, ", ")
}

// Edit is a manual change to a campaign draft. A nil Rule on an existing
// version preserves its current audience; creation requires a rule. A nil
// Localization keeps the base version's language setup.
type Edit struct {
	Name         string
	Rule         *segment.Rule
	SMS          *content.SMS
	Email        *content.Email
	Localization *language.Settings
	// ABTest is the B side; it replaces the base version's only when
	// ABTestSet, and a nil one then removes the A/B test.
	ABTest    *abtest.Test
	ABTestSet bool
}

const approveTimeout = "15s"

type staged struct {
	userID   int64
	channel  string
	subject  *string
	body     string
	hash     string
	city     *int64  // the recipient's city now; nil when unknown
	language *string // the variant's language; nil when not known
	variant  *string // the A/B arm; nil without an A/B test
}

// version is what a version sends.
type version struct {
	number int
	rule   segment.Rule
	sms    *content.SMS
	email  *content.Email
	loc    *language.Settings // nil: written before there were languages
	ab     *abtest.Test       // nil: no A/B test
}

func (s *Store) loadVersion(ctx context.Context, tx pgx.Tx, id int64) (version, error) {
	var v version
	err := tx.QueryRow(ctx, `SELECT version, segment_rule, sms, email, localization, ab_test FROM campaign_versions WHERE id = $1`, id).
		Scan(&v.number, &v.rule, &v.sms, &v.email, &v.loc, &v.ab)
	return v, err
}

// checkABTest checks an A/B test's B side against the A contents.
func checkABTest(ab *abtest.Test, a []language.Variant) []Issue {
	if ab == nil {
		return nil
	}
	var issues []Issue
	for _, is := range abtest.Validate(*ab, a) {
		issues = append(issues, Issue{Path: is.Path, Code: is.Code, Message: is.Message})
	}
	return issues
}

func (v version) settings() language.Settings {
	if v.loc == nil {
		return language.Settings{Mode: language.ModeSingle}
	}
	return *v.loc
}

func (v version) perRecipient() bool { return v.loc != nil && v.loc.Mode == language.ModePerRecipient }

// channels are the channels the version sends on.
func (v version) channels() (sms, email bool) {
	if v.perRecipient() {
		return v.loc.Channels()
	}
	return v.sms != nil, v.email != nil
}

// content is what a recipient in lang and arm gets: the variant, or the one
// content; in arm B, its B counterpart.
func (v version) content(lang, arm string) (*content.SMS, *content.Email) {
	if arm == abtest.B && v.ab != nil {
		b, _ := v.ab.Variant(lang)
		return b.SMS, b.Email
	}
	if v.perRecipient() {
		vr, _ := v.loc.Variant(lang)
		return vr.SMS, vr.Email
	}
	return v.sms, v.email
}

// path names a field of the content in lang and arm as the draft does.
func (v version) path(lang, arm, field string) string {
	if arm == abtest.B && v.ab != nil {
		i := slices.IndexFunc(v.ab.Variants, func(vr language.Variant) bool { return vr.Language == lang })
		return fmt.Sprintf("ab_test.variants[%d].%s", i, field)
	}
	if v.perRecipient() {
		i := slices.IndexFunc(v.loc.Variants, func(vr language.Variant) bool { return vr.Language == lang })
		return fmt.Sprintf("per_recipient.variants[%d].%s", i, field)
	}
	return field
}

// validate checks the rule, the content and the language setup again.
func (v version) validate(cat segment.Catalog) (segment.Rule, []Issue) {
	var issues []Issue
	vres := segment.Validate(v.rule, cat)
	for _, e := range vres.Errors {
		issues = append(issues, Issue{Path: "segment_rule." + e.Path, Code: e.Code, Message: e.Message})
	}
	if !v.perRecipient() {
		for _, e := range content.ValidateDraft(v.sms, v.email) {
			issues = append(issues, Issue{Path: e.Path, Code: e.Code, Message: e.Message})
		}
	}
	if v.loc != nil {
		for _, e := range language.Validate(*v.loc, countryKnown(cat.Geo)) {
			issues = append(issues, Issue{Path: e.Path, Code: e.Code, Message: e.Message})
		}
	}
	issues = append(issues, checkABTest(v.ab, abtest.Contents(v.loc, v.sms, v.email))...)
	return vres.Normalized, issues
}

func countryKnown(cat *geo.Catalog) func(string) bool {
	return func(code string) bool {
		if cat == nil {
			return false
		}
		_, ok := cat.Country(code)
		return ok
	}
}

// slot is a content: a language ("" when not known) and an A/B arm (""
// without an A/B test).
type slot struct{ lang, arm string }

// plan is who gets which message, worked out now.
type plan struct {
	batch     []staged
	counts    map[slot]map[string]ChannelCounts // content → channel; Selected counts staged messages
	waiting   map[string]int                    // group → recipients waiting for a decision
	excluded  map[string]int                    // group → recipients a decision left out
	unreached map[string]int                    // channel → recipients with no language and no reach: left out for consent
	issues    []Issue                           // rendered messages over a channel limit
	samples   map[[3]string]Sample              // [language, channel, arm] → the first rendered message
}

func (p *plan) bump(sl slot, channel string, f func(*ChannelCounts)) {
	if p.counts[sl] == nil {
		p.counts[sl] = map[string]ChannelCounts{}
	}
	c := p.counts[sl][channel]
	f(&c)
	p.counts[sl][channel] = c
}

func (p *plan) stage(sl slot, m staged) {
	p.batch = append(p.batch, m)
	p.bump(sl, m.channel, func(c *ChannelCounts) { c.Selected++ })
	key := [3]string{sl.lang, m.channel, sl.arm}
	if _, ok := p.samples[key]; !ok {
		p.samples[key] = Sample{Language: sl.lang, Channel: m.channel, Variant: sl.arm, Subject: m.subject, Body: m.body}
	}
}

// planDeliveries selects the recipients now and works out for each the
// language (language.Settings.Resolve), the A/B arm (abtest.Arm), the
// consent on each channel (consent check 1) and the message rendered with
// their real values.
func planDeliveries(ctx context.Context, tx pgx.Tx, v version, campaignID int64, rule segment.Rule, cat segment.Catalog, now time.Time) (*plan, error) {
	compiled, err := segment.Compile(rule, cat, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+approveTimeout+"'"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT u.id, u.first_name, u.last_name, u.city_geoname_id, u.admin1_code, u.language, u.country_code,
		       (`+segment.SMSReachable+`), (`+segment.EmailReachable+`)
		FROM users u WHERE `+compiled.Where+` ORDER BY u.id`, compiled.Args...)
	if err != nil {
		return nil, fmt.Errorf("select recipients: %w", err)
	}
	settings := v.settings()
	p := &plan{counts: map[slot]map[string]ChannelCounts{}, waiting: map[string]int{}, excluded: map[string]int{},
		unreached: map[string]int{}, samples: map[[3]string]Sample{}}
	sendsSMS, sendsEmail := v.channels()
	tooLong, example := map[string]int{}, map[string]string{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			id             int64
			first, last    string
			cityID         *int64
			admin1, pref   *string
			country        string
			smsOK, emailOK bool
		)
		if err := row.Scan(&id, &first, &last, &cityID, &admin1, &pref, &country, &smsOK, &emailOK); err != nil {
			return struct{}{}, err
		}
		r := settings.Resolve(pref, country)
		switch {
		case r.Unresolved && !(smsOK && sendsSMS) && !(emailOK && sendsEmail):
			// No message reaches them whatever the language: left out for
			// consent, never waiting for a decision.
			if sendsSMS {
				p.unreached["sms"]++
			}
			if sendsEmail {
				p.unreached["email"]++
			}
			return struct{}{}, nil
		case r.Unresolved:
			p.waiting[r.Group]++
			return struct{}{}, nil
		case r.Excluded:
			p.excluded[r.Group]++
			return struct{}{}, nil
		}
		var lang, arm *string
		if r.Language != "" {
			lang = &r.Language
		}
		sl := slot{lang: r.Language}
		if v.ab != nil {
			// One arm per recipient, whatever the channel.
			sl.arm = abtest.Arm(campaignID, id)
			arm = &sl.arm
		}
		sms, email := v.content(sl.lang, sl.arm)
		vars := content.Vars{FirstName: first, LastName: last, City: placeName(cat.Geo, cityID, admin1), Language: r.Language}
		if sms != nil {
			if !smsOK {
				p.bump(sl, "sms", func(c *ChannelCounts) { c.ExcludedConsent++ })
			} else {
				body := content.Render(sms.Text, vars)
				if len(content.CheckRendered("sms", "", body)) > 0 {
					path := v.path(sl.lang, sl.arm, "sms.text")
					tooLong[path]++
					example[path] = body
				}
				p.stage(sl, staged{userID: id, channel: "sms", body: body, hash: content.Hash("sms", "", body), city: cityID,
					language: lang, variant: arm})
			}
		}
		if email != nil {
			if !emailOK {
				p.bump(sl, "email", func(c *ChannelCounts) { c.ExcludedConsent++ })
			} else {
				subject, body := content.Render(email.Subject, vars), content.Render(email.Body, vars)
				if len(content.CheckRendered("email", subject, body)) > 0 {
					path := v.path(sl.lang, sl.arm, "email.subject")
					tooLong[path]++
					example[path] = subject
				}
				p.stage(sl, staged{userID: id, channel: "email", subject: &subject, body: body,
					hash: content.Hash("email", subject, body), city: cityID, language: lang, variant: arm})
			}
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read recipients: %w", err)
	}
	for path, n := range tooLong {
		if strings.HasSuffix(path, "sms.text") {
			p.issues = append(p.issues, Issue{Path: path, Code: "too_long", Message: fmt.Sprintf(
				"%d alıcıda kişiselleştirilmiş SMS %d karakteri aşıyor (örn. %d karakter: %q)",
				n, content.SMSMaxRunes, len([]rune(example[path])), example[path])})
		} else {
			p.issues = append(p.issues, Issue{Path: path, Code: "too_long", Message: fmt.Sprintf(
				"%d alıcıda kişiselleştirilmiş email konusu %d karakteri aşıyor (örn. %q)", n, content.SubjectMaxRunes, example[path])})
		}
	}
	slices.SortFunc(p.issues, func(a, b Issue) int { return strings.Compare(a.Path, b.Path) })
	return p, nil
}

// Approve approves the current version of a draft. There is no approval
// TTL: freshness comes from the current-version check and from working
// everything out again now. In one transaction it
//
//  1. locks the campaign row (serializing against concurrent revisions),
//  2. requires versionID to be the current version (StaleError otherwise),
//  3. re-validates the rule, the content and the language setup,
//  4. selects recipients now, works out each one's language and drops those
//     without consent or contact (consent check 1); recipients waiting for
//     a language decision stop the approval (DecisionError) before anything
//     is queued,
//  5. renders each message from the recipient's variant with real values and
//     checks the channel limits,
//  6. writes the deliveries, due now or at the scheduled moment; a message
//     (user, channel, content) already held by another delivery is skipped
//     as a duplicate,
//  7. marks the campaign approved.
//
// Approving an already approved version again returns the stored counts.
func (s *Store) Approve(ctx context.Context, campaignID, versionID int64, cat segment.Catalog, now time.Time) (ApproveResult, error) {
	return s.ApproveWith(ctx, campaignID, versionID, cat, now, ApproveOptions{})
}

// ApproveWith is Approve with the marketer's choices. A scheduled campaign
// is approved now: its content is rendered and its recipients chosen now,
// and only the sending waits; consent is checked again before each call.
func (s *Store) ApproveWith(ctx context.Context, campaignID, versionID int64, cat segment.Catalog, now time.Time, opts ApproveOptions) (ApproveResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApproveResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		status                       string
		currentID                    int64
		approvedID                   *int64
		approval, approved, byArmRaw []byte
		scheduledAt                  *time.Time
	)
	// Lock the campaign row alone, then read the version with a fresh
	// snapshot (see SaveDraft for why a joined FOR UPDATE is wrong here).
	err = tx.QueryRow(ctx, `
		SELECT status, current_version_id, approved_version_id, approval, approval_languages, approval_variants, scheduled_at
		FROM campaigns WHERE id = $1 FOR UPDATE`,
		campaignID).Scan(&status, &currentID, &approvedID, &approval, &approved, &byArmRaw, &scheduledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApproveResult{}, ErrNotFound
	}
	if err != nil {
		return ApproveResult{}, err
	}
	v, err := s.loadVersion(ctx, tx, currentID)
	if err != nil {
		return ApproveResult{}, err
	}
	res := ApproveResult{CampaignID: campaignID, VersionID: versionID, Channels: map[string]ChannelCounts{}}
	if status != "draft" {
		if approvedID != nil && *approvedID == versionID && approval != nil {
			res.Status, res.AlreadyApproved = status, true
			if err := json.Unmarshal(approval, &res.Channels); err != nil {
				return ApproveResult{}, err
			}
			if approved != nil {
				var al approvalLanguages
				if err := json.Unmarshal(approved, &al); err != nil {
					return ApproveResult{}, err
				}
				res.Languages, res.ExcludedLanguage = al.Languages, al.ExcludedLanguage
			}
			if byArmRaw != nil {
				if err := json.Unmarshal(byArmRaw, &res.Variants); err != nil {
					return ApproveResult{}, err
				}
			}
			res.ScheduledAt = inUTC(scheduledAt)
			return res, nil
		}
		return ApproveResult{}, ErrNotDraft
	}
	if currentID != versionID {
		return ApproveResult{}, &StaleError{CurrentVersion: v.number}
	}
	due := now
	if opts.SendAt != nil {
		at := opts.SendAt.UTC()
		if issues := checkSchedule(at, now); len(issues) > 0 {
			return ApproveResult{}, &ValidationError{Issues: issues}
		}
		due, res.ScheduledAt = at, &at
	}

	rule, issues := v.validate(cat)
	if len(issues) > 0 {
		return ApproveResult{}, &ValidationError{Issues: issues}
	}
	p, err := planDeliveries(ctx, tx, v, campaignID, rule, cat, now)
	if err != nil {
		return ApproveResult{}, err
	}
	if len(p.waiting) > 0 {
		return ApproveResult{}, &DecisionError{Groups: language.GroupCounts(p.waiting)}
	}
	if len(p.issues) > 0 {
		return ApproveResult{}, &ValidationError{Issues: p.issues}
	}
	if len(p.batch) == 0 {
		return ApproveResult{}, &ValidationError{Issues: []Issue{{Path: "segment_rule", Code: "empty_audience",
			Message: "Onaylanacak alıcı yok: kitlede bu kanallar için izni ve iletişim bilgisi olan kimse kalmadı"}}}
	}

	inserted, err := insertDeliveries(ctx, tx, campaignID, p.batch, due)
	if err != nil {
		return ApproveResult{}, err
	}
	res.Channels, res.Languages, res.Variants = p.split(inserted)
	res.ExcludedLanguage = language.GroupCounts(p.excluded)
	stats, err := json.Marshal(res.Channels)
	if err != nil {
		return ApproveResult{}, err
	}
	var byLanguage, byArm []byte
	if v.loc != nil {
		if byLanguage, err = json.Marshal(approvalLanguages{Languages: res.Languages, ExcludedLanguage: res.ExcludedLanguage}); err != nil {
			return ApproveResult{}, err
		}
	} else {
		res.Languages = nil // a version from before there were languages
	}
	if res.Variants != nil {
		if byArm, err = json.Marshal(res.Variants); err != nil {
			return ApproveResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE campaigns SET status = 'approved', approved_version_id = $2, approval = $3, approval_languages = $4,
		       approved_at = $5, scheduled_at = $6, approval_variants = $7, updated_at = now()
		WHERE id = $1`,
		campaignID, versionID, stats, byLanguage, now, res.ScheduledAt, byArm); err != nil {
		return ApproveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApproveResult{}, err
	}
	res.Status = "approved"
	return res, nil
}

// inUTC is t in UTC, for answers that promise UTC whatever the database
// session's time zone.
func inUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// split turns the staged counts into counts per channel, per language and,
// with an A/B test, per arm (nil without one), given how many messages of
// each (content, channel) were written (or, in a preview, would be); the
// rest were duplicates.
func (p *plan) split(written map[slot]map[string]int) (channels map[string]ChannelCounts,
	languages, arms map[string]map[string]ChannelCounts) {
	channels = map[string]ChannelCounts{}
	for ch, n := range p.unreached {
		c := channels[ch]
		c.ExcludedConsent += n
		channels[ch] = c
	}
	languages = map[string]map[string]ChannelCounts{}
	add := func(m map[string]map[string]ChannelCounts, key, ch string, c ChannelCounts) {
		if m[key] == nil {
			m[key] = map[string]ChannelCounts{}
		}
		sum := m[key][ch]
		sum.Selected += c.Selected
		sum.ExcludedConsent += c.ExcludedConsent
		sum.ExcludedDuplicate += c.ExcludedDuplicate
		m[key][ch] = sum
	}
	for sl, byChannel := range p.counts {
		for ch, c := range byChannel {
			n := written[sl][ch]
			c.ExcludedDuplicate, c.Selected = c.Selected-n, n
			add(languages, sl.lang, ch, c)
			if sl.arm != "" {
				if arms == nil {
					arms = map[string]map[string]ChannelCounts{}
				}
				add(arms, sl.arm, ch, c)
			}
			total := channels[ch]
			total.Selected += c.Selected
			total.ExcludedConsent += c.ExcludedConsent
			total.ExcludedDuplicate += c.ExcludedDuplicate
			channels[ch] = total
		}
	}
	return channels, languages, arms
}

// Preview is what approving a version would do now; a preview writes
// nothing. Unresolved groups would stop the approval; Issues are rendered
// messages over a channel limit, which would too.
type Preview struct {
	VersionID  int64                               `json:"version_id"`
	Mode       string                              `json:"mode,omitempty"`
	Channels   map[string]ChannelCounts            `json:"channels"`
	Languages  map[string]map[string]ChannelCounts `json:"languages"`
	Unresolved []language.GroupCount               `json:"unresolved"`
	Excluded   []language.GroupCount               `json:"excluded_language"`
	Countries  []language.CountryLanguage          `json:"country_languages"`
	Decisions  []language.Decision                 `json:"decisions"`
	Samples    []Sample                            `json:"samples"`
	Issues     []Issue                             `json:"issues"`
	// Variants splits the counts by A/B arm, then channel.
	Variants map[string]map[string]ChannelCounts `json:"variants,omitempty"`
}

// Sample is one message as a recipient of its language, channel and A/B
// arm gets it.
type Sample struct {
	Language string  `json:"language"`
	Channel  string  `json:"channel"`
	Variant  string  `json:"variant,omitempty"`
	Subject  *string `json:"subject,omitempty"`
	Body     string  `json:"body"`
}

// PreviewApproval works out what approving versionID would do now, the way
// Approve does, and writes nothing: the counts per language and channel,
// who would wait for a decision or be left out, who already got the same
// message, and one rendered message per language and channel.
func (s *Store) PreviewApproval(ctx context.Context, campaignID, versionID int64, cat segment.Catalog, now time.Time) (Preview, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Preview{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // nothing is written

	var (
		status    string
		currentID int64
	)
	err = tx.QueryRow(ctx, `SELECT status, current_version_id FROM campaigns WHERE id = $1`, campaignID).Scan(&status, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrNotFound
	}
	if err != nil {
		return Preview{}, err
	}
	if status != "draft" {
		return Preview{}, ErrNotDraft
	}
	v, err := s.loadVersion(ctx, tx, currentID)
	if err != nil {
		return Preview{}, err
	}
	if currentID != versionID {
		return Preview{}, &StaleError{CurrentVersion: v.number}
	}
	rule, issues := v.validate(cat)
	if len(issues) > 0 {
		return Preview{}, &ValidationError{Issues: issues}
	}
	p, err := planDeliveries(ctx, tx, v, campaignID, rule, cat, now)
	if err != nil {
		return Preview{}, err
	}
	written, err := wouldWrite(ctx, tx, p.batch)
	if err != nil {
		return Preview{}, err
	}
	settings := v.settings()
	out := Preview{VersionID: versionID, Unresolved: language.GroupCounts(p.waiting), Excluded: language.GroupCounts(p.excluded),
		Countries: settings.Countries, Decisions: settings.Decisions, Issues: p.issues}
	if v.loc != nil {
		out.Mode = v.loc.Mode
	}
	out.Channels, out.Languages, out.Variants = p.split(written)
	for _, m := range p.samples {
		out.Samples = append(out.Samples, m)
	}
	slices.SortFunc(out.Samples, func(a, b Sample) int {
		return cmp.Or(cmp.Compare(a.Language, b.Language), cmp.Compare(a.Variant, b.Variant), cmp.Compare(a.Channel, b.Channel))
	})
	return out, nil
}

// slotCounts collects (channel, language, arm) rows into counts per content
// and channel.
type slotCounts map[slot]map[string]int

func (w slotCounts) add(ch, lang, arm string, n int) {
	sl := slot{lang: lang, arm: arm}
	if w[sl] == nil {
		w[sl] = map[string]int{}
	}
	w[sl][ch] += n
}

// wouldWrite counts, per content and channel, the staged messages no other
// delivery holds: what insertDeliveries would write now.
func wouldWrite(ctx context.Context, tx pgx.Tx, batch []staged) (slotCounts, error) {
	users, channels, hashes := make([]int64, len(batch)), make([]string, len(batch)), make([]string, len(batch))
	langs, arms := make([]*string, len(batch)), make([]*string, len(batch))
	for i, b := range batch {
		users[i], channels[i], hashes[i], langs[i], arms[i] = b.userID, b.channel, b.hash, b.language, b.variant
	}
	rows, err := tx.Query(ctx, `
		SELECT s.channel, coalesce(s.lang, ''), coalesce(s.arm, ''), count(*)
		FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::text[]) AS s(user_id, channel, content_hash, lang, arm)
		WHERE NOT EXISTS (SELECT 1 FROM deliveries d
		                  WHERE d.user_id = s.user_id AND d.channel = s.channel AND d.content_hash = s.content_hash
		                    AND d.status NOT IN ('failed', 'skipped_no_consent'))
		GROUP BY 1, 2, 3`, users, channels, hashes, langs, arms)
	if err != nil {
		return nil, fmt.Errorf("count duplicates: %w", err)
	}
	out := slotCounts{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			ch, lang, arm string
			n             int
		)
		if err := row.Scan(&ch, &lang, &arm, &n); err != nil {
			return struct{}{}, err
		}
		out.add(ch, lang, arm, n)
		return struct{}{}, nil
	})
	return out, err
}

// insertDeliveries writes the staged messages, due from due on. ON CONFLICT
// DO NOTHING covers both the per-campaign key and the content-level key
// (ux_deliveries_message): a message the person already got, or will get,
// from another campaign is skipped. It returns the inserted count per
// content and channel.
func insertDeliveries(ctx context.Context, tx pgx.Tx, campaignID int64, batch []staged, due time.Time) (slotCounts, error) {
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE staged_deliveries (user_id BIGINT, channel TEXT, subject TEXT, body TEXT, content_hash TEXT,
		                                     city_geoname_id BIGINT, language TEXT, ab_variant TEXT)
		ON COMMIT DROP`); err != nil {
		return nil, err
	}
	rows := make([][]any, len(batch))
	for i, b := range batch {
		rows[i] = []any{b.userID, b.channel, b.subject, b.body, b.hash, b.city, b.language, b.variant}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"staged_deliveries"},
		[]string{"user_id", "channel", "subject", "body", "content_hash", "city_geoname_id", "language", "ab_variant"},
		pgx.CopyFromRows(rows)); err != nil {
		return nil, fmt.Errorf("stage deliveries: %w", err)
	}
	res, err := tx.Query(ctx, `
		INSERT INTO deliveries (campaign_id, user_id, channel, subject, body, content_hash, city_geoname_id, language, ab_variant,
		                        status, next_attempt_at)
		SELECT $1, user_id, channel, subject, body, content_hash, city_geoname_id, language, ab_variant, 'pending', $2
		FROM staged_deliveries ORDER BY user_id, channel
		ON CONFLICT DO NOTHING
		RETURNING channel, coalesce(language, ''), coalesce(ab_variant, '')`, campaignID, due)
	if err != nil {
		return nil, fmt.Errorf("insert deliveries: %w", err)
	}
	inserted := slotCounts{}
	if _, err = pgx.CollectRows(res, func(row pgx.CollectableRow) (struct{}, error) {
		var ch, lang, arm string
		err := row.Scan(&ch, &lang, &arm)
		inserted.add(ch, lang, arm, 1)
		return struct{}{}, err
	}); err != nil {
		return nil, err
	}
	// ON COMMIT DROP alone is not enough when the approval runs as a
	// savepoint of a longer transaction (the demo history): the next approval
	// in it would find the table still there.
	_, err = tx.Exec(ctx, `DROP TABLE staged_deliveries`)
	return inserted, err
}

// placeName fills {{city}}: the city when known, otherwise the province.
func placeName(cat *geo.Catalog, cityID *int64, admin1 *string) string {
	if cat == nil {
		return ""
	}
	if cityID != nil {
		if c, ok := cat.City(*cityID); ok {
			return c.Name
		}
	}
	if admin1 != nil {
		if a, ok := cat.Admin1(*admin1); ok {
			return a.Name
		}
	}
	return ""
}

// EditVersion saves a manual edit of a draft's content as a new version
// (source "user"). The audience rule and its count, and the language setup
// with its split, are kept from the base. baseVersionID must be the current
// version.
func (s *Store) EditVersion(ctx context.Context, campaignID, baseVersionID int64, e Edit) (Saved, error) {
	e.Rule, e.Localization = nil, nil
	return s.edit(ctx, campaignID, baseVersionID, e, nil, time.Time{})
}

// CreateManual validates a marketer's complete draft and creates version 1
// without making an LLM call or writing a synthetic chat message.
func (s *Store) CreateManual(ctx context.Context, chatID string, e Edit, cat segment.Catalog, now time.Time) (Saved, error) {
	rule, issues := validateManual(e, cat)
	issues = append(issues, validateContent(e, e.Localization, cat.Geo)...)
	issues = append(issues, checkABTest(e.ABTest, abtest.Contents(e.Localization, e.SMS, e.Email))...)
	if len(issues) > 0 {
		return Saved{}, &ValidationError{Issues: issues}
	}
	audience, err := segment.Count(ctx, s.pool, rule, cat, now)
	if err != nil {
		return Saved{}, err
	}
	languages, err := s.languages(ctx, rule, e.Localization, e.SMS, e.Email, cat, now)
	if err != nil {
		return Saved{}, err
	}
	return s.Drafts(Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, DraftCmd{
		Name: strings.TrimSpace(e.Name), Rule: rule, SMS: e.SMS, Email: e.Email,
		Audience: audience, ChangeSummary: "Manuel oluşturuldu", Localization: e.Localization, Languages: languages,
		ABTest: e.ABTest,
	})
}

// EditVersionWithRule saves a manual edit as a new version. A nil rule keeps
// the base rule and its count (old clients); a new rule is checked and
// counted. A nil language setup keeps the base one; a new one (a variant's
// text, a decision made in the approval dialog) is checked. The audience's
// language split is worked out again either way.
func (s *Store) EditVersionWithRule(ctx context.Context, campaignID, baseVersionID int64, e Edit, cat segment.Catalog, now time.Time) (Saved, error) {
	return s.edit(ctx, campaignID, baseVersionID, e, &cat, now)
}

// edit writes a manual edit; without a catalog (EditVersion) it keeps the
// rule, the language setup and their counts as they are.
func (s *Store) edit(ctx context.Context, campaignID, baseVersionID int64, e Edit, cat *segment.Catalog, now time.Time) (Saved, error) {
	var (
		chatID, status string
		currentID      int64
		version        int
		rule           segment.Rule
		audience       segment.Audience
		loc            *language.Settings
		languages      *language.Summary
		ab             *abtest.Test
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.chat_id::text, c.status, c.current_version_id, v.version, v.segment_rule, v.audience,
		       v.localization, v.language_summary, v.ab_test
		FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.id = $1`, campaignID).Scan(&chatID, &status, &currentID, &version, &rule, &audience, &loc, &languages, &ab)
	if errors.Is(err, pgx.ErrNoRows) {
		return Saved{}, ErrNotFound
	}
	if err != nil {
		return Saved{}, err
	}
	if status != "draft" {
		return Saved{}, ErrNotDraft
	}
	if currentID != baseVersionID {
		return Saved{}, &StaleError{CurrentVersion: version}
	}
	var issues []Issue
	if strings.TrimSpace(e.Name) == "" {
		issues = append(issues, Issue{Path: "name", Code: "required", Message: "kampanya adı boş olamaz"})
	}
	var geoCat *geo.Catalog
	if cat != nil {
		geoCat = cat.Geo
		if e.Rule != nil {
			res := segment.Validate(*e.Rule, *cat)
			for _, is := range res.Errors {
				issues = append(issues, Issue{Path: "segment_rule." + is.Path, Code: is.Code, Message: is.Message})
			}
			rule = res.Normalized
		}
	}
	if e.Localization != nil {
		loc = e.Localization
	}
	if e.ABTestSet {
		ab = e.ABTest
	}
	issues = append(issues, validateContent(e, loc, geoCat)...)
	issues = append(issues, checkABTest(ab, abtest.Contents(loc, e.SMS, e.Email))...)
	if len(issues) > 0 {
		return Saved{}, &ValidationError{Issues: issues}
	}
	if cat != nil {
		if e.Rule != nil {
			if audience, err = segment.Count(ctx, s.pool, rule, *cat, now); err != nil {
				return Saved{}, err
			}
		}
		if languages, err = s.languages(ctx, rule, loc, e.SMS, e.Email, *cat, now); err != nil {
			return Saved{}, err
		}
	}
	// SaveDraft re-checks the base version under the row lock. The edit
	// sends its whole A/B setup, so none means none.
	return s.Drafts(Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, DraftCmd{
		CampaignID: &campaignID, BaseVersion: &version, Name: strings.TrimSpace(e.Name), Rule: rule,
		SMS: e.SMS, Email: e.Email, Audience: audience, ChangeSummary: "Manuel düzenlendi",
		Localization: loc, Languages: languages, ABTest: ab, DropABTest: ab == nil,
	})
}

// languages splits rule's audience by content language under loc (nil for
// a draft without a language setup); sms and email are the draft's content
// for everyone.
func (s *Store) languages(ctx context.Context, rule segment.Rule, loc *language.Settings, sms *content.SMS, email *content.Email,
	cat segment.Catalog, now time.Time) (*language.Summary, error) {
	if loc == nil {
		return nil, nil
	}
	buckets, err := segment.LanguageBuckets(ctx, s.pool, rule, cat, now)
	if err != nil {
		return nil, err
	}
	sum := language.Summarize(*loc, sms, email, buckets)
	return &sum, nil
}

func validateManual(e Edit, cat segment.Catalog) (segment.Rule, []Issue) {
	var issues []Issue
	if strings.TrimSpace(e.Name) == "" {
		issues = append(issues, Issue{Path: "name", Code: "required", Message: "kampanya adı boş olamaz"})
	}
	var rule segment.Rule
	if e.Rule == nil {
		issues = append(issues, Issue{Path: "segment_rule", Code: "required", Message: "hedef kitle kuralı gerekli"})
	} else {
		res := segment.Validate(*e.Rule, cat)
		for _, is := range res.Errors {
			issues = append(issues, Issue{Path: "segment_rule." + is.Path, Code: is.Code, Message: is.Message})
		}
		rule = res.Normalized
	}
	return rule, issues
}

// validateContent checks a manual draft's content against its language
// setup: a per-recipient draft has its content in its variants only. A
// setup carried over from the base version was checked when it was saved.
func validateContent(e Edit, loc *language.Settings, cat *geo.Catalog) []Issue {
	var issues []Issue
	if loc != nil && loc.Mode == language.ModePerRecipient {
		if e.SMS != nil || e.Email != nil {
			issues = append(issues, Issue{Path: "sms", Code: "per_recipient_content",
				Message: "her alıcıya kendi dilinde giden taslağın içeriği yalnız dil varyantlarında olur"})
		}
	} else {
		for _, is := range content.ValidateDraft(e.SMS, e.Email) {
			issues = append(issues, Issue{Path: is.Path, Code: is.Code, Message: is.Message})
		}
	}
	if loc != nil && e.Localization != nil {
		for _, is := range language.Validate(*loc, countryKnown(cat)) {
			issues = append(issues, Issue{Path: is.Path, Code: is.Code, Message: is.Message})
		}
	}
	return issues
}
