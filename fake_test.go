package classifier_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// harmless is the policy the tests use: three classes about nothing anyone
// would refuse, so no test needs a text that is actually objectionable.
const harmless = `{
  "about": "A policy for tests.",
  "model": "jev-test",
  "threshold": 0.5,
  "classes": [
    {"id": "fruit", "says": "a fruit", "question": "Does this text mention a fruit?", "yes": "Apples, pears, plums.", "no": "Vegetables, and fruit-coloured things."},
    {"id": "weather", "says": "the weather", "question": "Does this text talk about the weather?", "threshold": 0.8},
    {"id": "colour", "says": "a colour", "question": "Does this text name a colour?"}
  ]
}`

// call is one System One request as the fake received it.
type call struct {
	Auth      string
	State     string                     `json:"state"`
	Model     string                     `json:"model"`
	Questions map[string]json.RawMessage `json:"questions"`
}

// fakeTypeSafe answers System One calls from a table: for each question, the
// probability its table gives a keyword found in the text, and 0.05 where none
// is. A test puts at most one of a question's keywords in a text. It keeps the
// last call it was sent, and counts them all.
type fakeTypeSafe struct {
	*httptest.Server
	answers map[string]map[string]float64
	calls   atomic.Int32
	last    atomic.Pointer[call]
}

func newFake(t *testing.T, answers map[string]map[string]float64) *fakeTypeSafe {
	t.Helper()
	f := &fakeTypeSafe{answers: answers}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var c call
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&c) != nil {
			http.Error(w, "not a System One call", http.StatusBadRequest)
			return
		}
		c.Auth = r.Header.Get("Authorization")
		f.last.Store(&c)
		type answer struct {
			Type string  `json:"type"`
			Noul float64 `json:"noul"`
		}
		out := map[string]answer{}
		for id := range c.Questions { // into a map, order-free
			p := 0.05
			for word, v := range f.answers[id] {
				if strings.Contains(c.State, word) {
					p = v
				}
			}
			out[id] = answer{Type: "noul", Noul: p}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-test-answered", "answers": out, "usage": map[string]int{"input_tokens": 100},
		})
	}))
	t.Cleanup(f.Close)
	return f
}
