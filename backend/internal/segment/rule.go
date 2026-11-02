// Package segment defines the audience rule DSL that Claude emits, validates
// it against a whitelist and compiles it to parameterized SQL. LLM output is
// never executed as SQL.
package segment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"smart-campaign-composer/backend/internal/geo"
)

// Rule is a single flat group of conditions joined by Match ("all" | "any").
type Rule struct {
	Match      string      `json:"match"`
	Conditions []Condition `json:"conditions"`
}

// Condition is either an attribute filter or an event filter (Kind).
type Condition struct {
	Kind string `json:"kind"`

	// attribute
	Field    string   `json:"field,omitempty"`
	Operator string   `json:"operator,omitempty"`
	Values   []string `json:"values,omitempty"`

	// location
	Places []PlaceRef `json:"places,omitempty"`

	// event
	EventType     string `json:"event_type,omitempty"`
	WithinDays    int    `json:"within_days,omitempty"`
	MinCount      int    `json:"min_count,omitempty"`
	Negate        bool   `json:"negate,omitempty"`
	NotFollowedBy string `json:"not_followed_by,omitempty"`
}

// PlaceRef is a place in a location condition. Claude may send a name, an
// ISO code or a GeoNames id (plus a country to disambiguate); after
// validation it carries the resolved canonical identifiers.
type PlaceRef struct {
	Level     string `json:"level"`
	Name      string `json:"name,omitempty"`
	Code      string `json:"code,omitempty"`
	GeonameID int64  `json:"geoname_id,omitempty"`
	Country   string `json:"country,omitempty"`
}

// Issue is a machine-readable validation finding, returned to Claude verbatim
// so it can repair its output.
type Issue struct {
	Path       string      `json:"path"`
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Candidates []geo.Place `json:"candidates,omitempty"`
}

// Result of Validate. Normalized is the rule with canonical values (resolved
// place codes, default min_count) and is what gets stored and compiled.
type Result struct {
	Errors     []Issue `json:"errors,omitempty"`
	Warnings   []Issue `json:"warnings,omitempty"`
	Normalized Rule    `json:"-"`
}

// OK reports whether the rule has no blocking errors.
func (r Result) OK() bool { return len(r.Errors) == 0 }

// ParseRule decodes a rule strictly: unknown keys and trailing data are errors.
func ParseRule(b []byte) (Rule, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Rule
	if err := dec.Decode(&r); err != nil {
		return Rule{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Rule{}, errors.New("segment rule: trailing data after JSON object")
	}
	return r, nil
}
