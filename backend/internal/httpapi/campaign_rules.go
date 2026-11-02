package httpapi

import (
	"net/http"
	"slices"

	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/segment"
)

type ruleFieldView struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Operators   []string `json:"operators"`
	Min         int      `json:"min,omitempty"`
	Max         int      `json:"max,omitempty"`
	Description string   `json:"description"`
}

type ruleEventView struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	NotFollowedBy []string `json:"not_followed_by"`
}

func (s *Server) campaignRuleCatalog(w http.ResponseWriter, _ *http.Request) {
	cat := s.d.Catalog
	fields := make([]ruleFieldView, 0, len(cat.Fields))
	for _, f := range cat.Fields {
		typ := "int"
		switch f.Type {
		case segment.TypeDecimal:
			typ = "decimal"
		case segment.TypeLanguage:
			typ = "language"
		}
		fields = append(fields, ruleFieldView{Name: f.Name, Type: typ, Operators: f.Operators,
			Min: f.Min, Max: f.Max, Description: f.Description})
	}
	slices.SortFunc(fields, func(a, b ruleFieldView) int { return cmpString(a.Name, b.Name) })
	events := make([]ruleEventView, 0, len(cat.Events))
	for name, description := range cat.Events {
		seq := cat.Sequences[name]
		if seq == nil {
			seq = []string{}
		}
		events = append(events, ruleEventView{Name: name, Description: description, NotFollowedBy: seq})
	}
	slices.SortFunc(events, func(a, b ruleEventView) int { return cmpString(a.Name, b.Name) })
	places := []segment.PlaceRef{}
	if cat.Geo != nil {
		for _, p := range cat.Geo.Places() {
			ref := segment.PlaceRef{Level: string(p.Level), Country: p.Country, Name: p.Name, GeonameID: p.GeonameID}
			switch p.Level {
			case geo.LevelCountry:
				ref.Code = p.Country
			case geo.LevelAdmin1:
				ref.Code = p.Admin1
			}
			places = append(places, ref)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": fields, "events": events, "languages": cat.Languages, "places": places,
		"max_conditions": cat.MaxConditions, "max_within_days": cat.MaxWithinDays,
		"max_min_count": cat.MaxMinCount, "max_list_values": cat.MaxListValues,
	})
}

func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
