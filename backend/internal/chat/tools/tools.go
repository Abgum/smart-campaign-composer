// Package tools defines the tools Claude uses to write campaign drafts and
// parses their input strictly.
package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/segment"
)

// SaveDraftName is the tool that writes campaign drafts.
const SaveDraftName = "save_campaign_draft"

// SaveDraftInput is the tool's input. A nil CampaignID creates a campaign.
// The same content for everyone is SMS/Email in ContentLanguage; content
// per recipient's language is PerRecipient instead.
type SaveDraftInput struct {
	CampaignID      *int64         `json:"campaign_id,omitempty"`
	BaseVersion     *int           `json:"base_version,omitempty"`
	Name            string         `json:"name"`
	SegmentRule     segment.Rule   `json:"segment_rule"`
	SMS             *content.SMS   `json:"sms,omitempty"`
	Email           *content.Email `json:"email,omitempty"`
	ContentLanguage string         `json:"content_language,omitempty"`
	PerRecipient    *PerRecipient  `json:"per_recipient,omitempty"`
	ABTest          *abtest.Test   `json:"ab_test,omitempty"`
	ChangeSummary   string         `json:"change_summary"`
}

// PerRecipient sends each recipient the variant in their language.
type PerRecipient struct {
	Variants  []language.Variant         `json:"variants"`
	Countries []language.CountryLanguage `json:"country_languages,omitempty"`
	Decisions []language.Decision        `json:"decisions,omitempty"`
}

// Settings is the draft's language setup.
func (in SaveDraftInput) Settings() language.Settings {
	if p := in.PerRecipient; p != nil {
		return language.Settings{Mode: language.ModePerRecipient, Variants: p.Variants, Countries: p.Countries, Decisions: p.Decisions}
	}
	return language.Settings{Mode: language.ModeSingle, Language: in.ContentLanguage}
}

// SaveDraftTool returns the tool definition. Every object is closed
// (additionalProperties:false) and enums come from the segment catalog, so
// strict mode and the Go validator agree on the shape. Business rules
// (limits, cities, sequences) are still checked by Go.
func SaveDraftTool() llm.Tool {
	cat := segment.DefaultCatalog()
	fields := make([]string, 0, len(cat.Fields))
	for f := range cat.Fields {
		fields = append(fields, f)
	}
	slices.Sort(fields)
	events := make([]string, 0, len(cat.Events))
	for e := range cat.Events {
		events = append(events, e)
	}
	slices.Sort(events)

	place := object(map[string]any{
		"level":      enum("country", "city"),
		"name":       map[string]any{"type": "string", "description": "Place name, or empty when using a code or GeoNames id."},
		"code":       map[string]any{"type": "string", "description": "ISO 3166-1 alpha-2 country code, or empty if unknown."},
		"geoname_id": map[string]any{"type": "integer", "description": "GeoNames city id, or 0 if unknown."},
		"country":    map[string]any{"type": "string", "description": "Country to disambiguate a city name, or empty if not needed."},
	}, "level", "name", "code", "geoname_id", "country")

	condition := object(map[string]any{
		"kind":            enum("attribute", "location", "event"),
		"places":          map[string]any{"type": "array", "items": place, "description": "Location places, or [] for attribute and event conditions."},
		"field":           enum(append([]string{""}, fields...)...),
		"operator":        enum("", "eq", "neq", "in", "not_in", "gte", "lte", "between"),
		"values":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Attribute values, or [] for location and event conditions."},
		"event_type":      enum(append([]string{""}, events...)...),
		"within_days":     map[string]any{"type": "integer", "description": "Event lookback in days, or 0 for attribute and location conditions."},
		"min_count":       map[string]any{"type": "integer"},
		"negate":          map[string]any{"type": "boolean"},
		"not_followed_by": enum(events...),
	}, "kind", "places", "field", "operator", "values", "event_type", "within_days")

	sms := object(map[string]any{"text": map[string]any{"type": "string"}}, "text")
	email := object(map[string]any{"subject": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}, "subject", "body")
	lang := enum(language.Supported...)
	perRecipient := object(map[string]any{
		"variants": map[string]any{"type": "array", "items": object(map[string]any{
			"language": lang, "sms": sms, "email": email,
		}, "language"), "description": "One variant per language that has recipients; every variant has the same channels."},
		"country_languages": map[string]any{"type": "array", "items": object(map[string]any{
			"country":  map[string]any{"type": "string", "description": "ISO 3166-1 alpha-2"},
			"language": lang,
		}, "country", "language"), "description": "For recipients with no recorded preference: the language of their country. " +
			"Use an empty array when there are no country mappings. Leave out a country whose language is not supported or that has several languages."},
		"decisions": map[string]any{"type": "array", "items": object(map[string]any{
			"group":    map[string]any{"type": "string", "description": "A waiting group from the last result, e.g. country:CA, variant:fr"},
			"action":   enum(language.ActionAssign, language.ActionExclude),
			"language": lang,
		}, "group", "action"), "description": "The user's decision for a waiting group: assign it to a variant or leave it out. " +
			"Use an empty array when there are no decisions. Only for groups the user was asked about and answered."},
	}, "variants", "country_languages", "decisions")

	abTest := object(map[string]any{
		"variants": map[string]any{"type": "array", "items": object(map[string]any{
			"language": lang, "sms": sms, "email": email,
		}, "language"), "description": "One B content for each A content, in its language: content_language for sms/email, " +
			"each variant's language for per_recipient. Same channels as its A; at least one text differs from A."},
	}, "variants")
	abTest["description"] = "Only when the user asks for an A/B test: the B side. sms/email or per_recipient stay A. " +
		"Every recipient gets A or B, the same on both channels; the system assigns them, about half each. " +
		"When the campaign has an ab_test, send it again with every change, B updated if the change concerns both versions. " +
		"A save without ab_test keeps the campaign's test as it is; only " + RemoveABTestName + " removes it."

	// Strict mode writes properties in schema order (required ones in the
	// order listed below, then optional ones; see llm/anthropic), and an
	// optional field the model passes over can never be added later. The
	// campaign and its base version are therefore required, null for a new
	// campaign, and listed first.
	nullableInt := func(description string) map[string]any {
		return map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}},
			"description": description}
	}
	schema := object(map[string]any{
		"campaign_id":  nullableInt("When changing an existing campaign, one of target.allowed_campaign_ids; null for a new campaign."),
		"base_version": nullableInt("The current version of the changed campaign, from target_campaigns; null for a new campaign."),
		"name":         map[string]any{"type": "string", "description": "Short campaign name, in the language of the user's message"},
		"segment_rule": object(map[string]any{
			"match":      enum("all", "any"),
			"conditions": map[string]any{"type": "array", "items": condition},
		}, "match", "conditions"),
		"sms": map[string]any{"anyOf": []any{sms, map[string]any{"type": "null"}},
			"description": "Provide SMS text when the user asks for SMS; otherwise null."},
		"email": map[string]any{"anyOf": []any{email, map[string]any{"type": "null"}},
			"description": "Provide an email subject and body when the user asks for email; otherwise null."},
		"content_language": map[string]any{"type": "string", "enum": append([]string{""}, language.Supported...),
			"description": "For one content for everyone, the requested content language, otherwise the language of the user's message. " +
				"Use an empty string only with per_recipient."},
		"per_recipient": perRecipient,
		"ab_test":       abTest,
		"change_summary": map[string]any{"type": "string",
			"description": "What changed in this version, in one sentence in the language of the user's message"},
	}, "campaign_id", "base_version", "name", "segment_rule", "sms", "email", "content_language", "change_summary")

	return llm.Tool{
		Name: SaveDraftName,
		Description: "Saves a campaign draft (audience rule and SMS/email content). " +
			"For a new campaign set campaign_id and base_version to null; when changing an existing campaign, send the whole draft with both. " +
			"For one content for everyone, fill requested sms/email and content_language; set unused channels to null. " +
			"For per_recipient, set sms and email to null and content_language to an empty string. " +
			"Returns validation errors, or the audience count and how it splits by language.",
		InputSchema: schema,
		Strict:      true,
	}
}

// RemoveABTestName is the tool that removes a campaign's A/B test.
const RemoveABTestName = "remove_ab_test"

// RemoveABTestInput is remove_ab_test's input.
type RemoveABTestInput struct {
	CampaignID    int64  `json:"campaign_id"`
	BaseVersion   int    `json:"base_version"`
	ChangeSummary string `json:"change_summary"`
}

// RemoveABTestTool returns remove_ab_test. Removing a test is a call of its
// own: a draft that leaves ab_test out keeps the test, so a forgotten field
// can never remove one.
func RemoveABTestTool() llm.Tool {
	return llm.Tool{
		Name: RemoveABTestName,
		Description: "Removes the A/B test from a campaign draft: saves a new version that is the current one without its B side; " +
			"nothing else changes, and every recipient then gets A. Call it only when the user explicitly asks to remove or stop the A/B test. " +
			"A save_campaign_draft without ab_test keeps the test, so this is the only way to remove it.",
		InputSchema: object(map[string]any{
			"campaign_id":  map[string]any{"type": "integer", "description": "The campaign, one of target.allowed_campaign_ids."},
			"base_version": map[string]any{"type": "integer", "description": "The current version of the campaign, from target_campaigns."},
			"change_summary": map[string]any{"type": "string",
				"description": "What changed in this version, in one sentence in the language of the user's message"},
		}, "campaign_id", "base_version", "change_summary"),
		Strict: false,
	}
}

func object(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func enum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

// ParseSaveDraft decodes tool input strictly: unknown keys at any depth,
// trailing data and malformed or truncated JSON are errors.
func ParseSaveDraft(raw []byte) (SaveDraftInput, error) {
	var in SaveDraftInput
	if err := decodeStrict(SaveDraftName, raw, &in); err != nil {
		return SaveDraftInput{}, err
	}
	return in, nil
}

// ParseRemoveABTest decodes remove_ab_test's input as strictly.
func ParseRemoveABTest(raw []byte) (RemoveABTestInput, error) {
	var in RemoveABTestInput
	if err := decodeStrict(RemoveABTestName, raw, &in); err != nil {
		return RemoveABTestInput{}, err
	}
	return in, nil
}

func decodeStrict(tool string, raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New(tool + ": trailing data after JSON object")
	}
	return nil
}
