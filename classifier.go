package classifier

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed policy/default.json
var defaultPolicy []byte

// Default is the policy this repository ships: ten classes, tuned against
// jev-1.13.0 on 2026-10-07. Every call parses it afresh, so a caller may change
// its copy freely.
func Default() Policy {
	p, err := Parse(defaultPolicy)
	if err != nil {
		panic(fmt.Sprintf("classifier: the built-in policy does not validate: %v", err)) // a test holds it valid
	}
	return p
}

// Classifier is a policy and the client its questions are put through.
type Classifier struct {
	Policy Policy
	Client *Client
}

// Classify puts the policy's questions about one text to its model, and reads
// the answers against its lines. An error means there is no verdict, which a
// caller moderating anything treats as a refusal rather than a pass.
func (c Classifier) Classify(ctx context.Context, text string) (Verdict, Answers, error) {
	a, err := c.Client.Ask(ctx, c.Policy.Model, text, c.Policy.Questions())
	if err != nil {
		return Verdict{}, Answers{}, err
	}
	v, err := c.Policy.Decide(a.Yes)
	if err != nil {
		return Verdict{}, a, err
	}
	return v, a, nil
}
