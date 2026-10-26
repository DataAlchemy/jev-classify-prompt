package classifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	classifier "github.com/DataAlchemy/jev-prompt-classifier"
)

// testKey stands in for an API key: the tests look for it where it must not be.
const testKey = "test-key-not-a-real-one"

var twoQuestions = []classifier.Question{
	{ID: "fruit", Instructions: "Does this text mention a fruit?", Yes: "Apples.", No: "Carrots."},
	{ID: "weather", Instructions: "Does this text talk about the weather?"},
}

// TestAskSendsSystemOnesShape: one call, in the shape TypeSafe documents --
// the text as the state, the model, and a "noul" per question under its id,
// with the criteria only where the question has them -- and the answers read
// back by question.
func TestAskSendsSystemOnesShape(t *testing.T) {
	f := newFake(t, map[string]map[string]float64{"fruit": {"apple": 0.9}})
	c := &classifier.Client{BaseURL: f.URL + "/", Key: testKey}
	a, err := c.Ask(context.Background(), "jev-test", "an apple a day", twoQuestions)
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "jev-test-answered" || a.Yes["fruit"] != 0.9 || a.Yes["weather"] != 0.05 || a.InputTokens != 100 {
		t.Errorf("answers %+v", a)
	}
	got := f.last.Load()
	if got.Auth != "Bearer "+testKey || got.State != "an apple a day" || got.Model != "jev-test" || len(got.Questions) != 2 {
		t.Fatalf("the call was %+v", got)
	}
	var fruit, weather map[string]any
	if json.Unmarshal(got.Questions["fruit"], &fruit) != nil || json.Unmarshal(got.Questions["weather"], &weather) != nil {
		t.Fatalf("the questions were %s", got.Questions)
	}
	criteria, _ := fruit["criteria"].(map[string]any)
	if fruit["type"] != "noul" || fruit["instructions"] != "Does this text mention a fruit?" ||
		criteria["true"] != "Apples." || criteria["false"] != "Carrots." {
		t.Errorf("the fruit question went as %v", fruit)
	}
	if _, has := weather["criteria"]; has || weather["type"] != "noul" {
		t.Errorf("a question with no criteria went as %v", weather)
	}
}

// TestAskSendsNoKeyItWasNotGiven: an empty key sends no Authorization header.
func TestAskSendsNoKeyItWasNotGiven(t *testing.T) {
	f := newFake(t, nil)
	if _, err := (&classifier.Client{BaseURL: f.URL}).Ask(context.Background(), "jev-test", "plain", twoQuestions); err != nil {
		t.Fatal(err)
	}
	if got := f.last.Load().Auth; got != "" {
		t.Errorf("sent the Authorization header %q with no key", got)
	}
}

// answering is a server that answers every call with one fixed body.
func answering(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(strings.ReplaceAll(body, "$AUTH", r.Header.Get("Authorization"))))
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// TestAskRefusesAnAnswerItCannotTrust: an answer missing a question, or
// giving one anything but a probability, is an error that is not worth
// retrying.
func TestAskRefusesAnAnswerItCannotTrust(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not JSON", `{"answers":`},
		{"a question unanswered", `{"answers":{"fruit":{"type":"noul","noul":0.1}}}`},
		{"not a noul", `{"answers":{"fruit":{"type":"text","noul":0.1},"weather":{"type":"noul","noul":0.1}}}`},
		{"no probability", `{"answers":{"fruit":{"type":"noul"},"weather":{"type":"noul","noul":0.1}}}`},
		{"above one", `{"answers":{"fruit":{"type":"noul","noul":1.5},"weather":{"type":"noul","noul":0.1}}}`},
		{"below zero", `{"answers":{"fruit":{"type":"noul","noul":-0.5},"weather":{"type":"noul","noul":0.1}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := answering(t, http.StatusOK, tc.body)
			_, err := (&classifier.Client{BaseURL: s.URL}).Ask(context.Background(), "jev-test", "plain", twoQuestions)
			var e *classifier.Error
			if !errors.As(err, &e) || e.Retryable {
				t.Fatalf("got %v, want a classifier.Error that is not retryable", err)
			}
		})
	}
}

// TestAskSaysWhatIsWorthRetrying: a rate limit or a fault on TypeSafe's side
// is worth another try and a refusal of the call itself is not; and no error
// carries the key, even where TypeSafe's own body repeats it.
func TestAskSaysWhatIsWorthRetrying(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{529, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusPaymentRequired, false},
	} {
		s, _ := answering(t, tc.status, `{"error":"you sent $AUTH"}`)
		_, err := (&classifier.Client{BaseURL: s.URL, Key: testKey}).Ask(context.Background(), "jev-test", "plain", twoQuestions)
		var e *classifier.Error
		if !errors.As(err, &e) || e.Status != tc.status || e.Retryable != tc.retryable {
			t.Errorf("%d: got %#v, want status %d, retryable %v", tc.status, err, tc.status, tc.retryable)
			continue
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("%d: the error carries the key: %v", tc.status, err)
		}
		if !strings.Contains(e.Body, "you sent") {
			t.Errorf("%d: the body for a person to read was %q", tc.status, e.Body)
		}
	}
}

// TestAskThatNeverArrives: a call that gets no answer at all is worth
// retrying, and a cancelled one returns the context's error.
func TestAskThatNeverArrives(t *testing.T) {
	s, _ := answering(t, http.StatusOK, `{}`)
	url := s.URL
	s.Close()
	_, err := (&classifier.Client{BaseURL: url}).Ask(context.Background(), "jev-test", "plain", twoQuestions)
	var e *classifier.Error
	if !errors.As(err, &e) || !e.Retryable || e.Status != 0 {
		t.Errorf("a call to a closed server came back %#v", err)
	}
	f := newFake(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&classifier.Client{BaseURL: f.URL}).Ask(ctx, "jev-test", "plain", twoQuestions); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call came back %v", err)
	}
}

// TestAskRefusesACallNothingCouldAnswer: a call with no model, no text or no
// usable questions is refused before anything is sent.
func TestAskRefusesACallNothingCouldAnswer(t *testing.T) {
	s, calls := answering(t, http.StatusOK, `{}`)
	c := &classifier.Client{BaseURL: s.URL}
	for _, tc := range []struct {
		name, model, text string
		questions         []classifier.Question
	}{
		{"no model", "", "plain", twoQuestions},
		{"no text", "jev-test", "  ", twoQuestions},
		{"no questions", "jev-test", "plain", nil},
		{"a question without instructions", "jev-test", "plain", []classifier.Question{{ID: "a"}}},
		{"a question without an id", "jev-test", "plain", []classifier.Question{{Instructions: "q"}}},
		{"an id twice", "jev-test", "plain", []classifier.Question{{ID: "a", Instructions: "q"}, {ID: "a", Instructions: "r"}}},
	} {
		if _, err := c.Ask(context.Background(), tc.model, tc.text, tc.questions); err == nil {
			t.Errorf("%s: the call was made", tc.name)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d calls reached the server", n)
	}
}
