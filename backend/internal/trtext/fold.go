// Package trtext holds Turkish-aware text helpers.
package trtext

import "strings"

var folder = strings.NewReplacer(
	"İ", "i", "I", "i", "ı", "i",
	"Ş", "s", "ş", "s", "Ğ", "g", "ğ", "g",
	"Ü", "u", "ü", "u", "Ö", "o", "ö", "o",
	"Ç", "c", "ç", "c", "Â", "a", "â", "a",
)

// Fold lowercases with Turkish rules and strips Turkish diacritics, so text
// typed on an ASCII keyboard matches canonical spellings
// ("ISTANBUL", "istanbul", "İstanbul" → "istanbul").
func Fold(s string) string {
	return strings.ToLower(folder.Replace(s))
}
