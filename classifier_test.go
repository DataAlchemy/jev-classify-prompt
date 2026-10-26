package classifier_test

import (
	"context"
	"net/http"
	"testing"

	classifier "github.com/DataAlchemy/jev-prompt-classifier"
)

// TestClassify: the policy's questions go out in one call, under the policy's
// model, and the answers come back read against its lines.
func TestClassify(t *testing.T) {
	f := newFake(t, map[string]map[string]float64{
		"fruit":   {"apple": 0.9},
		"weather": {"rain": 0.7},
	})
	c := classifier.Classifier{Policy: mustParse(t, harmless), Client: &classifier.Client{BaseURL: f.URL}}
	for _, tc := range []struct {
		text    string
		refused bool
		top     string
	}{
		{"an apple a day", true, "fruit"},
		{"light rain later", false, "weather"}, // 0.7 is under weather's own line of 0.8
		{"a quiet afternoon", false, "fruit"},  // all equal: the policy's order
	} {
		v, a, err := c.Classify(context.Background(), tc.text)
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if v.Refused() != tc.refused || v.Top().Class != tc.top || a.Model != "jev-test-answered" {
			t.Errorf("%q: refused %v as %q by %s, want %v as %q", tc.text, v.Refused(), v.Top().Class, a.Model, tc.refused, tc.top)
		}
		if got := f.last.Load(); got.Model != "jev-test" || len(got.Questions) != 3 {
			t.Errorf("%q went as %+v", tc.text, got)
		}
	}
	if n := f.calls.Load(); n != 3 {
		t.Errorf("%d calls for three texts", n)
	}
}

// TestClassifyWithoutAVerdict: an answer that leaves a class out is no
// verdict, and the error says so rather than the text passing.
func TestClassifyWithoutAVerdict(t *testing.T) {
	s, _ := answering(t, http.StatusOK, `{"answers":{"fruit":{"type":"noul","noul":0.1}}}`)
	c := classifier.Classifier{Policy: mustParse(t, harmless), Client: &classifier.Client{BaseURL: s.URL}}
	if v, _, err := c.Classify(context.Background(), "plain"); err == nil {
		t.Fatalf("got the verdict %+v from an incomplete answer", v)
	}
}
