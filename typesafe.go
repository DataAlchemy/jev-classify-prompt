package classifier

// typesafe.go is the one call this package makes: TypeSafe's System One
// endpoint, which takes a text (its "state") and a map of typed questions and
// answers each one. Every question here is a "noul", a yes-or-no question
// answered with the probability that the answer is yes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// DefaultBaseURL is TypeSafe's API.
const DefaultBaseURL = "https://api.typesafe.ai"

// systemOnePath is the endpoint jev answers questions on.
const systemOnePath = "/v1/systemone"

// DefaultTimeout bounds one call made without an http.Client of the caller's.
const DefaultTimeout = 60 * time.Second

// maxAnswerBytes bounds how much of an answer is read.
const maxAnswerBytes = 1 << 20

// Question is one yes-or-no question about a text.
type Question struct {
	// ID is the key the answer comes back under. It is not sent to the
	// model.
	ID string
	// Instructions is the question itself.
	Instructions string
	// Yes and No say what each answer covers. Either may be empty.
	Yes, No string
}

// Answers is what the model said about one text.
type Answers struct {
	// Model is the model that answered, as TypeSafe names it.
	Model string
	// Yes is the probability that each question's answer is yes, by
	// question id.
	Yes map[string]float64
	// InputTokens is what TypeSafe counted the call as, where it said.
	InputTokens int
}

// Client puts questions to jev at TypeSafe.
type Client struct {
	// BaseURL is TypeSafe's API; DefaultBaseURL where empty.
	BaseURL string
	// Key is the API key. It is sent in the Authorization header and
	// nowhere else, and no error carries it. Where it is empty no header
	// is sent.
	Key string
	// HTTP makes the calls; one with DefaultTimeout where nil.
	HTTP *http.Client
}

// Error is a call that did not come back with answers. It carries neither the
// key nor the text that was asked about.
type Error struct {
	// Status is the HTTP status TypeSafe answered with, and zero where the
	// call got no answer at all.
	Status int
	// Retryable is a failure worth trying again after a pause: a rate
	// limit, a fault on TypeSafe's side, or a call that never completed.
	Retryable bool
	// Reason says what went wrong.
	Reason string
	// Body is the start of what TypeSafe sent with a refusal, for a person
	// to read. Error leaves it out, because nothing promises that it does
	// not repeat the text, and an error is the kind of thing that gets
	// logged.
	Body string
	// Err is the underlying error, where there was one.
	Err error
}

func (e *Error) Error() string {
	msg := "classifier: " + e.Reason
	if e.Status != 0 {
		msg = fmt.Sprintf("classifier: TypeSafe answered %d: %s", e.Status, e.Reason)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Ask puts every question about the text to the model in one call, and
// returns the probability that each answer is yes. It makes one attempt; what
// to do about a Retryable failure is the caller's decision. An answer missing
// a question, or giving one anything but a probability from 0 to 1, is an
// error.
func (c *Client) Ask(ctx context.Context, model, text string, questions []Question) (Answers, error) {
	if err := checkCall(model, text, questions); err != nil {
		return Answers{}, err
	}
	type criteria struct {
		True  string `json:"true,omitempty"`
		False string `json:"false,omitempty"`
	}
	type noul struct {
		Type         string    `json:"type"`
		Instructions string    `json:"instructions"`
		Criteria     *criteria `json:"criteria,omitempty"`
	}
	qs := make(map[string]noul, len(questions))
	for _, q := range questions {
		n := noul{Type: "noul", Instructions: q.Instructions}
		if q.Yes != "" || q.No != "" {
			n.Criteria = &criteria{True: q.Yes, False: q.No}
		}
		qs[q.ID] = n
	}
	body, err := json.Marshal(struct {
		State     string          `json:"state"`
		Model     string          `json:"model"`
		Questions map[string]noul `json:"questions"`
	}{text, model, qs})
	if err != nil {
		return Answers{}, fmt.Errorf("classifier: encoding the call: %w", err)
	}
	base := strings.TrimSuffix(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+systemOnePath, bytes.NewReader(body))
	if err != nil {
		return Answers{}, fmt.Errorf("classifier: building the call: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Answers{}, ctx.Err()
		}
		return Answers{}, &Error{Retryable: true, Reason: "the call did not complete", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Answers{}, &Error{
			Status:    resp.StatusCode,
			Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Reason:    "the call was refused",
			Body:      strings.TrimSpace(string(excerpt)),
		}
	}
	var out struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBytes)).Decode(&out); err != nil {
		return Answers{}, &Error{Status: resp.StatusCode, Reason: "the answer was not in System One's shape", Err: err}
	}
	yes := make(map[string]float64, len(questions))
	for _, q := range questions { // read by the questions asked, so none is missed
		a, ok := out.Answers[q.ID]
		switch {
		case !ok:
			return Answers{}, &Error{Status: resp.StatusCode, Reason: fmt.Sprintf("question %q came back unanswered", q.ID)}
		case a.Type != "noul" || a.Noul == nil:
			return Answers{}, &Error{Status: resp.StatusCode, Reason: fmt.Sprintf("question %q was not answered with a yes-or-no probability", q.ID)}
		case math.IsNaN(*a.Noul) || *a.Noul < 0 || *a.Noul > 1:
			return Answers{}, &Error{Status: resp.StatusCode, Reason: fmt.Sprintf("question %q came back with %v, not a probability from 0 to 1", q.ID, *a.Noul)}
		}
		yes[q.ID] = *a.Noul
	}
	answeredBy := out.Model
	if answeredBy == "" {
		answeredBy = model
	}
	return Answers{Model: answeredBy, Yes: yes, InputTokens: out.Usage.InputTokens}, nil
}

// checkCall refuses a call nothing could answer.
func checkCall(model, text string, questions []Question) error {
	if strings.TrimSpace(model) == "" {
		return errors.New("classifier: no model is named")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("classifier: there is no text to ask about")
	}
	if len(questions) == 0 {
		return errors.New("classifier: there are no questions to ask")
	}
	seen := make([]string, 0, len(questions))
	for i, q := range questions {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("classifier: question %d has no id or no instructions", i)
		}
		if slices.Contains(seen, q.ID) {
			return fmt.Errorf("classifier: question id %q is used twice", q.ID)
		}
		seen = append(seen, q.ID)
	}
	return nil
}
