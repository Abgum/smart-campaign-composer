package segment

import (
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/language"
)

// ValueType tells the validator how to parse an attribute's values.
type ValueType int

const (
	TypeInt ValueType = iota
	TypeDecimal
	TypeLanguage
)

// FieldSpec describes one whitelisted user attribute.
type FieldSpec struct {
	Name        string
	Type        ValueType
	Operators   []string
	Min, Max    int // inclusive bounds for TypeInt
	Description string
}

// Catalog is the whitelist the validator and compiler work from. It is also
// rendered into the system prompt, so Claude and Go share one definition.
// Locations are not attributes: they are resolved against the geo catalog
// in "location" conditions.
type Catalog struct {
	Fields        map[string]FieldSpec
	Events        map[string]string   // event type → description
	Sequences     map[string][]string // event type → allowed not_followed_by values
	Languages     []string
	Geo           *geo.Catalog
	MaxConditions int
	MaxWithinDays int
	MaxMinCount   int
	MaxListValues int
}

// DefaultCatalog returns the production whitelist.
func DefaultCatalog() Catalog {
	return Catalog{
		Fields: map[string]FieldSpec{
			"age":             {Name: "age", Type: TypeInt, Operators: []string{"eq", "gte", "lte", "between"}, Min: 0, Max: 120, Description: "Age in years"},
			"total_orders":    {Name: "total_orders", Type: TypeInt, Operators: []string{"eq", "gte", "lte", "between"}, Min: 0, Max: 100000, Description: "Total number of orders"},
			"signup_days_ago": {Name: "signup_days_ago", Type: TypeInt, Operators: []string{"gte", "lte", "between"}, Min: 0, Max: 36500, Description: "Days since sign-up"},
			"total_spent":     {Name: "total_spent", Type: TypeDecimal, Operators: []string{"gte", "lte", "between"}, Description: "Total spent (TRY)"},
			"language":        {Name: "language", Type: TypeLanguage, Operators: []string{"eq", "in"}, Description: "Preferred language"},
		},
		Events: map[string]string{
			"cart_add":     "Added a product to the cart",
			"purchase":     "Purchase",
			"product_view": "Viewed a product",
		},
		Sequences: map[string][]string{
			"cart_add":     {"purchase"},
			"product_view": {"cart_add", "purchase"},
		},
		Languages:     language.Supported,
		Geo:           geo.Default(),
		MaxConditions: 10,
		MaxWithinDays: 365,
		MaxMinCount:   100,
		MaxListValues: 20,
	}
}
