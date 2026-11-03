package product

import (
	"strings"
	"unicode"
)

const (
	maxQueryTerms = 6
	maxTermLength = 40
)

// tsQuery turns free text into a safe PostgreSQL to_tsquery expression that
// matches every word by prefix: "kaos hit" → "kaos:* & hit:*". Terms keep
// only letters and digits, so user input can never break the query syntax.
// It returns "" when nothing searchable remains.
func tsQuery(text string) string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, maxQueryTerms)
	for _, f := range fields {
		if len(terms) == maxQueryTerms {
			break
		}
		if r := []rune(f); len(r) > maxTermLength {
			f = string(r[:maxTermLength])
		}
		terms = append(terms, f+":*")
	}
	return strings.Join(terms, " & ")
}
