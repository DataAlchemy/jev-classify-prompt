package classifier_test

import (
	"math"
	"strings"
	"testing"

	classifier "github.com/DataAlchemy/jev-prompt-classifier"
)

func mustParse(t *testing.T, s string) classifier.Policy {
	t.Helper()
	p, err := classifier.Parse([]byte(s))
	if err != nil {
		t.Fatalf("the test policy does not parse: %v", err)
	}
	return p
}

// TestParseRefusesAPolicyThatCouldNotBeAsked: each broken policy is refused,
// saying what is wrong.
func TestParseRefusesAPolicyThatCouldNotBeAsked(t *testing.T) {
	for _, tc := range []struct{ name, policy, says string }{
		{"not JSON", `{`, "does not parse"},
		{"no model", `{"threshold":0.5,"classes":[{"id":"a","question":"q"}]}`, "names no model"},
		{"threshold zero", `{"model":"m","threshold":0,"classes":[{"id":"a","question":"q"}]}`, "not a probability"},
		{"threshold one", `{"model":"m","threshold":1,"classes":[{"id":"a","question":"q"}]}`, "not a probability"},
		{"no classes", `{"model":"m","threshold":0.5,"classes":[]}`, "no classes"},
		{"class without id", `{"model":"m","threshold":0.5,"classes":[{"question":"q"}]}`, "needs an id and a question"},
		{"class without question", `{"model":"m","threshold":0.5,"classes":[{"id":"a"}]}`, "needs an id and a question"},
		{"a class twice", `{"model":"m","threshold":0.5,"classes":[{"id":"a","question":"q"},{"id":"a","question":"r"}]}`, "declared twice"},
		{"a class's line out of range", `{"model":"m","threshold":0.5,"classes":[{"id":"a","question":"q","threshold":1.5}]}`, `class "a" has a threshold`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := classifier.Parse([]byte(tc.policy))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("got %v, want an error saying %q", err, tc.says)
			}
		})
	}
}

// TestParseIgnoresWhatACallerAdds: a policy file may carry a caller's own
// fields beside the classifier's, such as the penalty a class carries.
func TestParseIgnoresWhatACallerAdds(t *testing.T) {
	p := mustParse(t, `{"model":"m","threshold":0.5,"classes":[{"id":"a","question":"q","bar":"permanent","steps":["24h"],"because":"why"}]}`)
	if len(p.Classes) != 1 || p.Classes[0].ID != "a" {
		t.Fatalf("parsed %+v", p)
	}
}

// TestDecide: a class matches at its line, its own where it has one; the
// verdict ranks every class; and it turns on the most probable class that
// matched, which need not be the most probable of all.
func TestDecide(t *testing.T) {
	p := mustParse(t, harmless)
	for _, tc := range []struct {
		name    string
		yes     map[string]float64
		refused bool
		top     string
		ranked  []string
	}{
		{"nothing near", map[string]float64{"fruit": 0.1, "weather": 0.2, "colour": 0.05}, false, "weather", []string{"weather", "fruit", "colour"}},
		{"exactly at the policy's line", map[string]float64{"fruit": 0.5, "weather": 0, "colour": 0}, true, "fruit", []string{"fruit", "weather", "colour"}},
		{"under a class's own line", map[string]float64{"fruit": 0.1, "weather": 0.79, "colour": 0}, false, "weather", []string{"weather", "fruit", "colour"}},
		{"at a class's own line", map[string]float64{"fruit": 0.1, "weather": 0.8, "colour": 0}, true, "weather", []string{"weather", "fruit", "colour"}},
		{"the most probable did not match", map[string]float64{"fruit": 0.6, "weather": 0.79, "colour": 0}, true, "fruit", []string{"weather", "fruit", "colour"}},
		{"equal scores keep the policy's order", map[string]float64{"fruit": 0.3, "weather": 0.3, "colour": 0.3}, false, "fruit", []string{"fruit", "weather", "colour"}},
		{"answers nobody asked for are ignored", map[string]float64{"fruit": 0.1, "weather": 0.1, "colour": 0.9, "other": 1}, true, "colour", []string{"colour", "fruit", "weather"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := p.Decide(tc.yes)
			if err != nil {
				t.Fatal(err)
			}
			if v.Refused() != tc.refused || v.Top().Class != tc.top {
				t.Errorf("refused %v, top %q; want %v, %q", v.Refused(), v.Top().Class, tc.refused, tc.top)
			}
			var got []string
			for _, s := range v.Ranked {
				got = append(got, s.Class)
			}
			if strings.Join(got, ",") != strings.Join(tc.ranked, ",") {
				t.Errorf("ranked %v, want %v", got, tc.ranked)
			}
			if len(v.Matched()) > 0 != tc.refused {
				t.Errorf("matched %v while refused is %v", v.Matched(), tc.refused)
			}
		})
	}
}

// TestDecideRefusesAnAnswerItCannotTrust: a class left out, or answered with
// something other than a probability, is an error and never a pass.
func TestDecideRefusesAnAnswerItCannotTrust(t *testing.T) {
	p := mustParse(t, harmless)
	for _, tc := range []struct {
		name string
		yes  map[string]float64
	}{
		{"a class unanswered", map[string]float64{"fruit": 0.1, "weather": 0.1}},
		{"below zero", map[string]float64{"fruit": -0.1, "weather": 0.1, "colour": 0.1}},
		{"above one", map[string]float64{"fruit": 1.1, "weather": 0.1, "colour": 0.1}},
		{"not a number", map[string]float64{"fruit": math.NaN(), "weather": 0.1, "colour": 0.1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v, err := p.Decide(tc.yes); err == nil {
				t.Fatalf("decided %+v from an answer that cannot be trusted", v)
			}
		})
	}
}

// TestDigest: the digest moves with the model and with a question's wording,
// which change the answers, and not with a threshold, which does not.
func TestDigest(t *testing.T) {
	base := mustParse(t, harmless)
	moved := mustParse(t, harmless)
	moved.Threshold = 0.6
	moved.Classes[1].Threshold = 0.9
	if base.Digest() != moved.Digest() {
		t.Error("moving a threshold changed the digest, so kept answers would be asked again for nothing")
	}
	reworded := mustParse(t, harmless)
	reworded.Classes[0].Yes = "Apples."
	other := mustParse(t, harmless)
	other.Model = "jev-other"
	for name, p := range map[string]classifier.Policy{"rewording": reworded, "another model": other} {
		if p.Digest() == base.Digest() {
			t.Errorf("%s left the digest alone, so stale answers would be read as fresh", name)
		}
	}
}

// TestTheDefaultPolicy: the built-in policy is the tuned one -- jev-1.13.0,
// ten classes, every line at 0.5 but crime's at 0.7 -- and every class says
// what it is and what each answer covers.
func TestTheDefaultPolicy(t *testing.T) {
	p := classifier.Default()
	if p.Model != "jev-1.13.0" || p.Threshold != 0.5 {
		t.Fatalf("the default policy pins %s at %v, want jev-1.13.0 at 0.5", p.Model, p.Threshold)
	}
	var ids []string
	for _, c := range p.Classes {
		ids = append(ids, c.ID)
		if c.Says == "" || c.Yes == "" || c.No == "" {
			t.Errorf("class %q does not say what it is, or what an answer covers", c.ID)
		}
		want := 0.5
		if c.ID == "crime" {
			want = 0.7
		}
		if p.Line(c) != want {
			t.Errorf("class %q refuses at %v, want %v", c.ID, p.Line(c), want)
		}
	}
	const want = "violence,weapons,rioting,cyberattack,sexual,targeting,hate,crime,tracking,jailbreak"
	if got := strings.Join(ids, ","); got != want {
		t.Errorf("classes %s, want %s", got, want)
	}
	p.Classes[0].Question = "changed"
	if classifier.Default().Classes[0].Question == "changed" {
		t.Error("a caller's change to its copy reached the next caller's")
	}
}
