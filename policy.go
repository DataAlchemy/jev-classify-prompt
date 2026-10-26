// Package classifier asks jev, TypeSafe's question-answering model, whether a
// text falls in any of a policy's classes, and reads the answers against the
// policy's lines.
//
// A policy is a list of classes, and each class is one yes-or-no question about
// the text, worded as the exact condition, with what a yes and a no cover
// beside it: jev answers the question that was written, not the one that was
// meant. Every question goes to jev in one call, jev answers each with the
// probability that its answer is yes, and a class matches when that
// probability reaches the class's line. A text is refused when any class
// matches.
//
// An answer that leaves a class out, or gives it something other than a
// probability, is an error and never a pass: a caller that cannot get an
// answer holds the text rather than letting it through unread.
package classifier

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
)

// Policy is a set of classes and the lines they are refused at.
type Policy struct {
	// About says what the policy is for and how it was tuned. It is never
	// sent to the model.
	About string `json:"about,omitempty"`
	// Model is the jev model the questions are put to, pinned by version
	// (jev-1.13.0 rather than jev-latest), so an answer does not change
	// under a release nobody chose.
	Model string `json:"model"`
	// Threshold is the probability every class is refused at, unless the
	// class carries its own.
	Threshold float64 `json:"threshold"`
	// Classes are asked in this order, and the order breaks ties between
	// equally probable classes.
	Classes []Class `json:"classes"`
}

// Class is one kind of text the policy refuses.
type Class struct {
	// ID names the class: the key its answer comes back under. TypeSafe
	// does not show it to the model, so it needs only to be unique.
	ID string `json:"id"`
	// Says is the class in a few words, for a sentence a person reads.
	Says string `json:"says,omitempty"`
	// Question is put to the model, with Yes and No saying what each
	// answer covers, the boundary cases included.
	Question string `json:"question"`
	Yes      string `json:"yes,omitempty"`
	No       string `json:"no,omitempty"`
	// Threshold is this class's own line, where it differs from the
	// policy's: a class whose harmless texts score high needs more
	// certainty before it refuses. Zero means the policy's.
	Threshold float64 `json:"threshold,omitempty"`
}

// Parse reads a policy and validates it. Fields it does not know are ignored,
// so one policy file can also carry what its caller adds to a class, such as
// the penalty a match carries.
func Parse(data []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("classifier: the policy does not parse: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Load reads and validates a policy file.
func Load(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("classifier: reading the policy: %w", err)
	}
	return Parse(data)
}

// Validate refuses a policy that could not be asked, or whose answers could
// not be read.
func (p Policy) Validate() error {
	if strings.TrimSpace(p.Model) == "" {
		return errors.New("classifier: the policy names no model")
	}
	if !probability(p.Threshold) {
		return fmt.Errorf("classifier: a threshold of %v is not a probability strictly between 0 and 1", p.Threshold)
	}
	if len(p.Classes) == 0 {
		return errors.New("classifier: a policy with no classes refuses nothing")
	}
	seen := make([]string, 0, len(p.Classes))
	for i, c := range p.Classes {
		switch {
		case strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Question) == "":
			return fmt.Errorf("classifier: class %d needs an id and a question", i)
		case slices.Contains(seen, c.ID):
			return fmt.Errorf("classifier: class %q is declared twice", c.ID)
		case c.Threshold != 0 && !probability(c.Threshold):
			return fmt.Errorf("classifier: class %q has a threshold of %v, not a probability strictly between 0 and 1", c.ID, c.Threshold)
		}
		seen = append(seen, c.ID)
	}
	return nil
}

// probability is whether v can be a line: strictly between 0 and 1, since a
// line at 0 refuses everything and one at 1 refuses nothing.
func probability(v float64) bool { return v > 0 && v < 1 }

// Line is the probability a class is refused at: its own threshold, or the
// policy's.
func (p Policy) Line(c Class) float64 {
	if c.Threshold > 0 {
		return c.Threshold
	}
	return p.Threshold
}

// Class is the class by id.
func (p Policy) Class(id string) (Class, bool) {
	for _, c := range p.Classes {
		if c.ID == id {
			return c, true
		}
	}
	return Class{}, false
}

// Questions is the policy as one call asks it: a question a class, in the
// policy's order.
func (p Policy) Questions() []Question {
	out := make([]Question, 0, len(p.Classes))
	for _, c := range p.Classes {
		out = append(out, Question{ID: c.ID, Instructions: c.Question, Yes: c.Yes, No: c.No})
	}
	return out
}

// Digest identifies everything that decides what the model answers: the model
// and every question exactly as it is asked. The thresholds are not in it,
// since they are applied after the answers come back, so answers kept under a
// digest can be read against new lines without asking again.
func (p Policy) Digest() string {
	raw, err := json.Marshal(struct {
		Model     string     `json:"model"`
		Questions []Question `json:"questions"`
	}{p.Model, p.Questions()})
	if err != nil {
		panic(fmt.Sprintf("classifier: encoding strings cannot fail: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Score is one class's answer about one text.
type Score struct {
	// Class is the class's id.
	Class string
	// P is the probability that the class's answer is yes.
	P float64
	// Line is the probability the class is refused at.
	Line float64
}

// Matched is whether the class reached its line.
func (s Score) Matched() bool { return s.P >= s.Line }

// Verdict is a policy's reading of one text's answers.
type Verdict struct {
	// Ranked is every class of the policy, most probable first, in the
	// policy's order where two are equally probable.
	Ranked []Score
}

// Refused is whether any class reached its line.
func (v Verdict) Refused() bool { return slices.ContainsFunc(v.Ranked, Score.Matched) }

// Matched is every class that reached its line, most probable first.
func (v Verdict) Matched() []Score {
	var out []Score
	for _, s := range v.Ranked {
		if s.Matched() {
			out = append(out, s)
		}
	}
	return out
}

// Top is the class the verdict turns on: the most probable class that reached
// its line where any did, and otherwise the most probable class of all, which
// is how near a text that passed came.
func (v Verdict) Top() Score {
	for _, s := range v.Ranked {
		if s.Matched() {
			return s
		}
	}
	if len(v.Ranked) == 0 {
		return Score{}
	}
	return v.Ranked[0]
}

// Decide reads a model's answers, by class id, against the policy. Every class
// must have been answered with a probability from 0 to 1; an answer that
// leaves one out, or gives it anything else, cannot be trusted, and is an
// error rather than a pass. Answers to questions the policy does not ask are
// ignored.
func (p Policy) Decide(yes map[string]float64) (Verdict, error) {
	ranked := make([]Score, 0, len(p.Classes))
	for _, c := range p.Classes { // the policy's order, which breaks ties below
		v, ok := yes[c.ID]
		if !ok {
			return Verdict{}, fmt.Errorf("classifier: the model left class %q unanswered", c.ID)
		}
		if math.IsNaN(v) || v < 0 || v > 1 {
			return Verdict{}, fmt.Errorf("classifier: class %q came back with %v, not a probability from 0 to 1", c.ID, v)
		}
		ranked = append(ranked, Score{Class: c.ID, P: v, Line: p.Line(c)})
	}
	slices.SortStableFunc(ranked, func(a, b Score) int { return cmp.Compare(b.P, a.P) })
	return Verdict{Ranked: ranked}, nil
}
