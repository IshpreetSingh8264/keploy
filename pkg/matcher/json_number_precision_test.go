package matcher

import (
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestJSONNumberPrecision pins the JSON number semantics of the string entry
// points: a number keeps its literal through decoding, comparison and diffing,
// while numbers that denote the same value still match.
func TestJSONNumberPrecision(t *testing.T) {
	tests := []struct {
		name           string
		expected       string
		actual         string
		noise          map[string][]string
		ignoreOrdering bool
		wantMatch      bool
		wantExact      bool
	}{
		{
			name:     "issue 5038 repro: large integer loses no digits",
			expected: `{"id":1234567890123456789}`,
			actual:   `{"id":1234567890123456700}`,
		},
		{
			name:      "the same large integer matches",
			expected:  `{"id":1234567890123456789}`,
			actual:    `{"id":1234567890123456789}`,
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "one above 2^53 differs from 2^53",
			expected:  `{"id":9007199254740993}`,
			actual:    `{"id":9007199254740992}`,
			wantMatch: false,
			wantExact: false,
		},
		{
			name:      "integer and decimal forms of one value match",
			expected:  `{"n":1}`,
			actual:    `{"n":1.0}`,
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "exponent and plain forms of one value match",
			expected:  `{"n":1e3}`,
			actual:    `{"n":1000}`,
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "negative zero matches zero",
			expected:  `{"n":-0}`,
			actual:    `{"n":0}`,
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "nested object keeps large integer precision",
			expected:  `{"outer":{"id":1234567890123456789}}`,
			actual:    `{"outer":{"id":1234567890123456700}}`,
			wantMatch: false,
			wantExact: false,
		},
		{
			name:      "nested object matches the same large integer",
			expected:  `{"outer":{"id":1234567890123456789}}`,
			actual:    `{"outer":{"id":1234567890123456789}}`,
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "nested array keeps large integer precision",
			expected:  `{"items":[{"id":1234567890123456789}]}`,
			actual:    `{"items":[{"id":1234567890123456700}]}`,
			wantMatch: false,
			wantExact: false,
		},
		{
			name:           "ignoreOrdering matches reordered large integers",
			expected:       `{"ids":[1234567890123456789,2]}`,
			actual:         `{"ids":[2,1234567890123456789]}`,
			ignoreOrdering: true,
			wantMatch:      true,
			wantExact:      true,
		},
		{
			name:           "ignoreOrdering still rejects a changed large integer",
			expected:       `{"ids":[1234567890123456789,2]}`,
			actual:         `{"ids":[1234567890123456700,2]}`,
			ignoreOrdering: true,
			wantMatch:      false,
			wantExact:      false,
		},
		{
			name:      "a noise regex sees the literal large integer",
			expected:  `{"id":1}`,
			actual:    `{"id":1234567890123456789}`,
			noise:     map[string][]string{"id": {`^1234567890123456789$`}},
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "a noise regex written for the old float rendering matches a recorded 1.0",
			expected:  `{"n":1.0}`,
			actual:    `{"n":999}`,
			noise:     map[string][]string{"n": {`^1$`}},
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "a noise regex sees 1e3 as its float rendering 1000",
			expected:  `{"n":1e3}`,
			actual:    `{"n":999}`,
			noise:     map[string][]string{"n": {`^1000$`}},
			wantMatch: true,
			wantExact: true,
		},
		{
			name:      "a noise regex naming every digit matches a recorded large integer",
			expected:  `{"n":1234567890123456789}`,
			actual:    `{"n":999}`,
			noise:     map[string][]string{"n": {`^1234567890123456789$`}},
			wantMatch: true,
			wantExact: true,
		},
		{
			name:           "ignoreOrdering treats decimal and plain integers as equal",
			expected:       `{"items":[1.0,2.0]}`,
			actual:         `{"items":[1,2]}`,
			ignoreOrdering: true,
			wantMatch:      true,
			wantExact:      true,
		},
		{
			name:      "a number and a string of it still differ",
			expected:  `{"n":1}`,
			actual:    `{"n":"1"}`,
			wantMatch: false,
			wantExact: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp, act := tt.expected, tt.actual
			validated, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
			if err != nil {
				t.Fatalf("ValidateAndMarshalJSON: %v", err)
			}
			if !validated.IsIdentical() {
				t.Fatalf("top-level JSON types differ: %q vs %q", exp, act)
			}

			got, err := JSONDiffWithNoiseControl(validated, tt.noise, tt.ignoreOrdering, zap.NewNop())
			if err != nil {
				t.Fatalf("JSONDiffWithNoiseControl: %v", err)
			}
			if got.Matches() != tt.wantMatch {
				t.Errorf("Matches() = %v, want %v", got.Matches(), tt.wantMatch)
			}
			if got.IsExact() != tt.wantExact {
				t.Errorf("IsExact() = %v, want %v", got.IsExact(), tt.wantExact)
			}
		})
	}
}

// TestValidateAndMarshalJSONPreservesNumberLiteral guards the round trip the
// matcher performs before comparing: re-marshalling must not round a large
// integer through float64.
func TestValidateAndMarshalJSONPreservesNumberLiteral(t *testing.T) {
	const literal = "1234567890123456789"
	exp := `{"id":` + literal + `}`
	act := exp
	if _, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act); err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	for name, got := range map[string]string{"expected": exp, "actual": act} {
		if !strings.Contains(got, literal) {
			t.Errorf("%s lost the literal %s: %s", name, literal, got)
		}
	}
}

// TestFormatJSONNumberLiteral pins the rendering shared by noise matching,
// diffs and ordering keys: number spellings that denote one float64 render as
// formatJSONNumber always has, while an integer beyond 2^53 keeps every digit.
func TestFormatJSONNumberLiteral(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1.0", "1"},
		{"1", "1"},
		{"1e0", "1"},
		{"1e3", "1000"},
		{"1e21", "1e+21"},
		{"0.10", "0.1"},
		{"1234567890123456789", "1234567890123456789"},
		{"-1234567890123456789", "-1234567890123456789"},
		{"9007199254740992", "9007199254740992"},
		{"9007199254740993", "9007199254740993"},
	} {
		if got := formatJSONNumberLiteral(json.Number(c.in)); got != c.want {
			t.Errorf("formatJSONNumberLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	const big = "1234567890123456789"
	if got := formatJSONNumberLiteral(json.Number(big)); len(got) != 19 || !strings.Contains(got, big) {
		t.Errorf("large integer rendered as %q, want all 19 digits of %s", got, big)
	}
}
