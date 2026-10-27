// Command jev-classify asks jev about each line of a file, against a policy,
// and writes a row for each line: how it came out, the class it turned on, and
// how probable that class was.
//
// Usage:
//
//	jev-classify [flags] [file]
//
// It reads standard input where no file, or "-", is given, and skips blank
// lines. Rows go to standard output as tab-separated values, in the file's
// order, under a header:
//
//	line  verdict  class  score  line_for_class  second_class  second_score  text
//
// line is the line's number in the file. verdict is refused where any class
// reached its line and passed otherwise; class is the most probable class that
// reached its line, or for a line that passed the most probable of all; and
// second_class is the most probable of the rest. With -borderline, a passed
// line whose class scored at or above that probability is labelled borderline
// for a person to review: the policy itself has only passed and refused. A
// line jev could not be asked about is unanswered.
//
// A summary goes to standard error.
//
// With -scores, every answer is kept in a file, under the digest of the text
// and the digest of the model and questions that produced it. A line already
// answered there is not asked again, so moving a threshold re-reads the kept
// answers for nothing, while rewording a question asks again, as it must. The
// file holds digests of the texts, never the texts.
//
// With -expect passed or -expect refused, the lines that came out otherwise
// are listed by number and the exit status is 1. An unanswered line makes it 2.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	classifier "github.com/DataAlchemy/jev-classify-prompt"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// backoff is the pause before a line's second attempt, doubled before each
// attempt after it.
var backoff = time.Second

// maxLine bounds one line of input.
const maxLine = 1 << 20

// entry is one line of input and what became of it.
type entry struct {
	n      int    // the line's number in the file, from 1
	text   string // the line, trimmed
	sum    string // the text's digest: its key among kept answers
	yes    map[string]float64
	asked  bool
	tokens int
	err    error
	class  string // the class its verdict turned on
}

// kept is the -scores file.
type kept struct {
	Version int `json:"version"`
	// Answers is every answer kept: by the digest of the model and
	// questions that produced it (classifier.Policy.Digest), then by the
	// digest of the text, then by class.
	Answers map[string]map[string]map[string]float64 `json:"answers"`
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("jev-classify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	policyPath := fs.String("policy", "", "the policy `file`; the built-in policy where none is given")
	keyFile := fs.String("key-file", getenv("TYPESAFE_KEY_FILE"), "the `file` holding TypeSafe's API key, by default $TYPESAFE_KEY_FILE; where neither is set, $TYPESAFE_API_KEY is the key")
	baseURL := fs.String("base-url", classifier.DefaultBaseURL, "TypeSafe's API")
	parallel := fs.Int("parallel", 4, "how many calls may be in flight at once")
	attempts := fs.Int("attempts", 5, "how many tries a line gets while TypeSafe is busy or failing")
	scoresPath := fs.String("scores", "", "a `file` keeping every answer: read before asking, written after")
	borderline := fs.Float64("borderline", 0, "label a passed line whose class scored at or above this `probability` borderline")
	expect := fs.String("expect", "", "passed or refused: list the lines that came out otherwise, and exit 1 if any did")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: jev-classify [flags] [file]\n\n"+
			"Asks jev about each line of the file, or of standard input where none is given,\n"+
			"against a policy, and writes a tab-separated row for each line to standard output.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "jev-classify: "+format+"\n", a...)
		return 2
	}
	switch {
	case fs.NArg() > 1:
		return fail("one file at a time, and %d were given", fs.NArg())
	case *parallel < 1:
		return fail("-parallel must be at least 1")
	case *attempts < 1:
		return fail("-attempts must be at least 1")
	case *borderline < 0 || *borderline >= 1:
		return fail("-borderline must be a probability from 0, which is off, up to 1")
	case *expect != "" && *expect != "passed" && *expect != "refused":
		return fail("-expect is passed or refused, not %q", *expect)
	}

	policy := classifier.Default()
	if *policyPath != "" {
		p, err := classifier.Load(*policyPath)
		if err != nil {
			return fail("%v", err)
		}
		policy = p
	}

	in := stdin
	if fs.NArg() == 1 && fs.Arg(0) != "-" {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return fail("%v", err)
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	entries, err := read(in)
	if err != nil {
		return fail("reading the input: %v", err)
	}

	store := kept{Version: 1, Answers: map[string]map[string]map[string]float64{}}
	if *scoresPath != "" {
		if store, err = load(*scoresPath); err != nil {
			return fail("%v", err)
		}
	}
	digest := policy.Digest()
	answers := store.Answers[digest]
	if answers == nil {
		answers = map[string]map[string]float64{}
		store.Answers[digest] = answers
	}
	var todo []*entry
	for _, e := range entries {
		if yes, ok := answers[e.sum]; ok && complete(policy, yes) {
			e.yes = yes
			continue
		}
		todo = append(todo, e)
	}

	if len(todo) > 0 {
		key, err := readKey(*keyFile, getenv)
		if err != nil {
			return fail("%v", err)
		}
		client := &classifier.Client{BaseURL: *baseURL, Key: key}
		askAll(ctx, client, policy, todo, *parallel, *attempts)
		for _, e := range todo {
			if e.err == nil {
				answers[e.sum] = e.yes
			}
		}
	}
	if *scoresPath != "" && len(todo) > 0 {
		if err := save(*scoresPath, store); err != nil {
			fmt.Fprintf(stderr, "jev-classify: the answers could not be kept: %v\n", err)
		}
	}

	verdicts, err := write(stdout, policy, entries, *borderline)
	if err != nil {
		return fail("writing the rows: %v", err)
	}
	return summarise(stderr, entries, verdicts, *expect)
}

// read is every non-blank line of the input, numbered as the file numbers it.
func read(in io.Reader) ([]*entry, error) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	var out []*entry
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		out = append(out, &entry{n: n, text: text, sum: hex.EncodeToString(sum[:])})
	}
	return out, sc.Err()
}

// complete is whether kept answers answer every class the policy asks.
func complete(p classifier.Policy, yes map[string]float64) bool {
	for _, c := range p.Classes {
		if _, ok := yes[c.ID]; !ok {
			return false
		}
	}
	return true
}

// readKey is TypeSafe's key, from its file or the environment. It is never
// printed, and no error here carries it.
func readKey(path string, getenv func(string) string) (string, error) {
	if path == "" {
		if key := strings.TrimSpace(getenv("TYPESAFE_API_KEY")); key != "" {
			return key, nil
		}
		return "", errors.New("there are lines to ask about and no API key: give -key-file, or set TYPESAFE_KEY_FILE or TYPESAFE_API_KEY")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("the API key file %s is empty", path)
	}
	return key, nil
}

// askAll asks about every entry, parallel at a time.
func askAll(ctx context.Context, c *classifier.Client, p classifier.Policy, todo []*entry, parallel, attempts int) {
	qs := p.Questions()
	work := make(chan *entry)
	var wg sync.WaitGroup
	for range min(parallel, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range work {
				a, err := ask(ctx, c, p.Model, e.text, qs, attempts)
				e.asked = true
				if err != nil {
					e.err = err
					continue
				}
				e.yes, e.tokens = a.Yes, a.InputTokens
			}
		}()
	}
	for _, e := range todo {
		work <- e
	}
	close(work)
	wg.Wait()
}

// ask puts one line's questions, trying again after a doubling pause while
// TypeSafe says it is busy or failing.
func ask(ctx context.Context, c *classifier.Client, model, text string, qs []classifier.Question, attempts int) (classifier.Answers, error) {
	pause := backoff
	for attempt := 1; ; attempt++ {
		a, err := c.Ask(ctx, model, text, qs)
		var e *classifier.Error
		if err == nil || attempt >= attempts || !errors.As(err, &e) || !e.Retryable {
			return a, err
		}
		select {
		case <-ctx.Done():
			return classifier.Answers{}, ctx.Err()
		case <-time.After(pause):
		}
		pause *= 2
	}
}

// write is a row a line, in the input's order, and each line's verdict:
// refused, borderline, passed or unanswered.
func write(out io.Writer, p classifier.Policy, entries []*entry, borderline float64) ([]string, error) {
	w := bufio.NewWriter(out)
	fmt.Fprintln(w, "line\tverdict\tclass\tscore\tline_for_class\tsecond_class\tsecond_score\ttext")
	verdicts := make([]string, len(entries))
	for i, e := range entries {
		var v classifier.Verdict
		err := e.err
		if err == nil {
			v, err = p.Decide(e.yes)
		}
		if err != nil {
			e.err = err
			verdicts[i] = "unanswered"
			fmt.Fprintf(w, "%d\tunanswered\t\t\t\t\t\t%s\n", e.n, cell(e.text))
			continue
		}
		top := v.Top()
		e.class = top.Class
		verdicts[i] = "passed"
		switch {
		case v.Refused():
			verdicts[i] = "refused"
		case borderline > 0 && top.P >= borderline:
			verdicts[i] = "borderline"
		}
		second, secondP := "", ""
		for _, s := range v.Ranked {
			if s.Class != top.Class {
				second, secondP = s.Class, fmt.Sprintf("%.3f", s.P)
				break
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%.3f\t%g\t%s\t%s\t%s\n",
			e.n, verdicts[i], top.Class, top.P, top.Line, second, secondP, cell(e.text))
	}
	return verdicts, w.Flush()
}

// cell is a text as one tab-separated field.
func cell(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ").Replace(s)
}

// summarise writes the counts to standard error and is the exit status.
func summarise(stderr io.Writer, entries []*entry, verdicts []string, expect string) int {
	count := map[string]int{}
	asked, tokens := 0, 0
	var problems []string
	for i, e := range entries {
		count[verdicts[i]]++
		if e.asked {
			asked++
			tokens += e.tokens
		}
		if verdicts[i] == "unanswered" && len(problems) < 3 {
			problems = append(problems, fmt.Sprintf("line %d: %s", e.n, describe(e.err)))
		}
	}
	fmt.Fprintf(stderr, "jev-classify: %d lines: %d refused", len(entries), count["refused"])
	if count["borderline"] > 0 {
		fmt.Fprintf(stderr, ", %d borderline", count["borderline"])
	}
	fmt.Fprintf(stderr, ", %d passed", count["passed"])
	if count["unanswered"] > 0 {
		fmt.Fprintf(stderr, ", %d unanswered", count["unanswered"])
	}
	fmt.Fprintln(stderr)
	if count["refused"] > 0 {
		fmt.Fprintf(stderr, "jev-classify: refused, by class: %s\n", refusedBy(entries, verdicts))
	}
	fmt.Fprintf(stderr, "jev-classify: asked jev about %d lines and read %d from kept answers; %s input tokens\n",
		asked, len(entries)-asked, thousands(tokens))
	for _, p := range problems {
		fmt.Fprintf(stderr, "jev-classify: unanswered, %s\n", p)
	}

	code := 0
	if expect != "" {
		var off []string
		for i, e := range entries {
			got := verdicts[i]
			if got == "unanswered" {
				continue
			}
			if (expect == "refused") != (got == "refused") {
				off = append(off, strconv.Itoa(e.n))
			}
		}
		if len(off) > 0 {
			fmt.Fprintf(stderr, "jev-classify: expected every line %s, and %d came out otherwise: line %s\n",
				expect, len(off), strings.Join(off, ", "))
			code = 1
		} else {
			fmt.Fprintf(stderr, "jev-classify: every answered line came out %s, as expected\n", expect)
		}
	}
	if count["unanswered"] > 0 {
		code = 2
	}
	return code
}

// refusedBy is how many refused lines each class turned on, most first.
func refusedBy(entries []*entry, verdicts []string) string {
	byClass := map[string]int{} // a lookup only: order holds the order
	var order []string
	for i, e := range entries {
		if verdicts[i] != "refused" {
			continue
		}
		if byClass[e.class] == 0 {
			order = append(order, e.class)
		}
		byClass[e.class]++
	}
	slices.SortStableFunc(order, func(a, b string) int { return byClass[b] - byClass[a] })
	parts := make([]string, 0, len(order))
	for _, c := range order {
		parts = append(parts, fmt.Sprintf("%s %d", c, byClass[c]))
	}
	return strings.Join(parts, ", ")
}

// describe is an unanswered line's reason, with what TypeSafe said where it
// said anything: this is the person's own terminal, so the body may be shown.
func describe(err error) string {
	var e *classifier.Error
	if errors.As(err, &e) && e.Body != "" {
		return fmt.Sprintf("%v (TypeSafe said: %s)", err, e.Body)
	}
	return err.Error()
}

// thousands is n with its thousands separated.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// load reads the -scores file, or starts one where there is none.
func load(path string) (kept, error) {
	k := kept{Version: 1, Answers: map[string]map[string]map[string]float64{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return kept{}, fmt.Errorf("reading the kept answers: %w", err)
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return kept{}, fmt.Errorf("the kept answers in %s do not parse, so they are left as they are: %w", path, err)
	}
	if k.Version != 1 {
		return kept{}, fmt.Errorf("the kept answers in %s are version %d, and this reads version 1", path, k.Version)
	}
	if k.Answers == nil {
		k.Answers = map[string]map[string]map[string]float64{}
	}
	return k, nil
}

// save writes the -scores file whole, through a temporary file beside it, so
// an interrupted write leaves the old one standing.
func save(path string, k kept) error {
	raw, err := json.Marshal(k)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".jev-classify-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
