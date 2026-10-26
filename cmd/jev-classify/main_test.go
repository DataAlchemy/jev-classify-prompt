package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// harmless is a policy about nothing anyone would refuse, so no test needs an
// objectionable text.
const harmless = `{
  "model": "jev-test",
  "threshold": 0.5,
  "classes": [
    {"id": "fruit", "question": "Does this text mention a fruit?"},
    {"id": "weather", "question": "Does this text talk about the weather?", "threshold": 0.8},
    {"id": "colour", "question": "Does this text name a colour?"}
  ]
}`

const testKey = "test-key-not-a-real-one"

// input has a blank line, so the rows' numbers must skip it.
const input = "an apple a day\n\nlight rain later\na quiet afternoon\n"

func init() { backoff = time.Millisecond }

// fake answers System One calls: fruit for "apple", weather at 0.7 for
// "rain", and 0.05 otherwise. It answers 503 while failing is above zero.
type fake struct {
	*httptest.Server
	calls   atomic.Int32
	failing atomic.Int32
	auth    atomic.Value
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.auth.Store(r.Header.Get("Authorization"))
		if f.failing.Add(-1) >= 0 {
			http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
			return
		}
		var c struct {
			State     string                     `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&c)
		table := map[string]map[string]float64{"fruit": {"apple": 0.9}, "weather": {"rain": 0.7}}
		out := map[string]any{}
		for id := range c.Questions {
			p := 0.05
			for word, v := range table[id] {
				if strings.Contains(c.State, word) {
					p = v
				}
			}
			out[id] = map[string]any{"type": "noul", "noul": p}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": out, "usage": map[string]int{"input_tokens": 100}})
	}))
	t.Cleanup(f.Close)
	return f
}

// writeFile writes a file in a test's directory and returns its path.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runWith runs the command with an environment of env only.
func runWith(env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errs, func(k string) string { return env[k] })
	return code, out.String(), errs.String()
}

// TestRows: a row a non-blank line, in the file's order and numbered as the
// file numbers them, each naming the class it turned on; and a summary that
// never shows the key.
func TestRows(t *testing.T) {
	f := newFake(t)
	policy := writeFile(t, t.TempDir(), "policy.json", harmless)
	code, out, errs := runWith(map[string]string{"TYPESAFE_API_KEY": testKey}, input,
		"-policy", policy, "-base-url", f.URL, "-borderline", "0.6")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := "line\tverdict\tclass\tscore\tline_for_class\tsecond_class\tsecond_score\ttext\n" +
		"1\trefused\tfruit\t0.900\t0.5\tweather\t0.050\tan apple a day\n" +
		"3\tborderline\tweather\t0.700\t0.8\tfruit\t0.050\tlight rain later\n" +
		"4\tpassed\tfruit\t0.050\t0.5\tweather\t0.050\ta quiet afternoon\n"
	if out != want {
		t.Errorf("rows:\n%s\nwant:\n%s", out, want)
	}
	for _, s := range []string{"3 lines: 1 refused, 1 borderline, 1 passed", "refused, by class: fruit 1", "asked jev about 3 lines and read 0 from kept answers; 300 input tokens"} {
		if !strings.Contains(errs, s) {
			t.Errorf("the summary does not say %q:\n%s", s, errs)
		}
	}
	if f.calls.Load() != 3 || f.auth.Load() != "Bearer "+testKey {
		t.Errorf("%d calls, the last with %q", f.calls.Load(), f.auth.Load())
	}
	if strings.Contains(out+errs, testKey) {
		t.Error("the key was printed")
	}
}

// TestKeptAnswers: answers kept with -scores are not asked for again, so a
// moved threshold costs nothing; a reworded question is asked again; and the
// file holds digests of the texts, not the texts.
func TestKeptAnswers(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	policy := writeFile(t, dir, "policy.json", harmless)
	scores := filepath.Join(dir, "scores.json")
	env := map[string]string{"TYPESAFE_API_KEY": testKey}
	args := []string{"-policy", policy, "-base-url", f.URL, "-scores", scores}

	_, first, _ := runWith(env, input, args...)
	if f.calls.Load() != 3 {
		t.Fatalf("%d calls the first time", f.calls.Load())
	}
	code, second, errs := runWith(nil, input, args...) // no key: nothing needs asking
	if code != 0 || second != first || f.calls.Load() != 3 {
		t.Fatalf("the second run made %d calls in all, exit %d: %s", f.calls.Load(), code, errs)
	}
	if !strings.Contains(errs, "asked jev about 0 lines and read 3 from kept answers") {
		t.Errorf("the summary was %s", errs)
	}

	writeFile(t, dir, "policy.json", strings.Replace(harmless, `"threshold": 0.8`, `"threshold": 0.6`, 1))
	_, moved, _ := runWith(nil, input, args...)
	if f.calls.Load() != 3 || !strings.Contains(moved, "3\trefused\tweather\t0.700\t0.6\t") {
		t.Errorf("after moving a line, %d calls and rows:\n%s", f.calls.Load(), moved)
	}

	writeFile(t, dir, "policy.json", strings.Replace(harmless, "mention a fruit", "name a fruit", 1))
	if code, _, errs := runWith(env, input, args...); code != 0 || f.calls.Load() != 6 {
		t.Errorf("after rewording a question, %d calls in all, exit %d: %s", f.calls.Load(), code, errs)
	}

	raw, err := os.ReadFile(scores)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "apple") {
		t.Error("the kept answers hold a text")
	}
}

// TestAKeyOnlyToAsk: a line that needs asking, with no key anywhere, stops the
// run before anything is sent.
func TestAKeyOnlyToAsk(t *testing.T) {
	f := newFake(t)
	code, _, errs := runWith(nil, input, "-base-url", f.URL)
	if code != 2 || !strings.Contains(errs, "no API key") || f.calls.Load() != 0 {
		t.Errorf("exit %d after %d calls: %s", code, f.calls.Load(), errs)
	}
	key := writeFile(t, t.TempDir(), "key", testKey+"\n")
	if code, _, errs := runWith(map[string]string{"TYPESAFE_KEY_FILE": key}, input, "-base-url", f.URL); code != 0 || f.auth.Load() != "Bearer "+testKey {
		t.Errorf("with a key file: exit %d, sent %q: %s", code, f.auth.Load(), errs)
	}
}

// TestExpect: the lines that came out otherwise are listed by number, and the
// exit status says so.
func TestExpect(t *testing.T) {
	f := newFake(t)
	policy := writeFile(t, t.TempDir(), "policy.json", harmless)
	env := map[string]string{"TYPESAFE_API_KEY": testKey}
	code, _, errs := runWith(env, input, "-policy", policy, "-base-url", f.URL, "-expect", "refused")
	if code != 1 || !strings.Contains(errs, "expected every line refused, and 2 came out otherwise: line 3, 4") {
		t.Errorf("exit %d: %s", code, errs)
	}
	code, _, errs = runWith(env, "a quiet afternoon\n", "-policy", policy, "-base-url", f.URL, "-expect", "passed")
	if code != 0 || !strings.Contains(errs, "came out passed, as expected") {
		t.Errorf("exit %d: %s", code, errs)
	}
}

// TestBusy: a busy TypeSafe is tried again, and one that stays busy leaves
// the line unanswered, says what TypeSafe said, and exits 2.
func TestBusy(t *testing.T) {
	f := newFake(t)
	policy := writeFile(t, t.TempDir(), "policy.json", harmless)
	env := map[string]string{"TYPESAFE_API_KEY": testKey}
	f.failing.Store(2)
	code, out, errs := runWith(env, "an apple a day\n", "-policy", policy, "-base-url", f.URL, "-attempts", "3")
	if code != 0 || f.calls.Load() != 3 || !strings.Contains(out, "1\trefused\tfruit") {
		t.Errorf("after two busy answers: exit %d, %d calls: %s%s", code, f.calls.Load(), out, errs)
	}
	f.failing.Store(100)
	code, out, errs = runWith(env, "an apple a day\n", "-policy", policy, "-base-url", f.URL, "-attempts", "2")
	if code != 2 || !strings.Contains(out, "1\tunanswered\t") || !strings.Contains(errs, "TypeSafe answered 503") || !strings.Contains(errs, "overloaded") {
		t.Errorf("staying busy: exit %d:\n%s%s", code, out, errs)
	}
}

// TestUsage: what cannot be run is refused with exit 2.
func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		{"a", "b"},
		{"-expect", "maybe"},
		{"-borderline", "1"},
		{"-parallel", "0"},
		{"-attempts", "0"},
		{"-policy", filepath.Join(t.TempDir(), "missing.json")},
		{"-no-such-flag"},
	} {
		if code, _, _ := runWith(nil, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
