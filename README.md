# jev-prompt-classifier

Asks **jev**, TypeSafe's question-answering model, whether a text falls in any class of a policy,
and reads the answers against the policy's lines. It is a Go library and a command-line tool, with
nothing beyond the standard library.

It began as the moderation in [Human42](https://human42.ai), where every thought, conversation
turn and appeal goes through it before anything else sees it.

## How it decides

- A **policy** is a list of classes. Each class is one yes-or-no question about the text, worded
  as the exact condition, with what a yes and a no cover beside it. jev answers the question that
  was written, not the one that was meant, so the boundary cases are spelled out.
- All of a policy's questions go to jev in **one call**. jev answers each one independently, with
  the probability that its answer is yes.
- A class **matches** when its probability reaches its line: the policy's threshold, or the
  class's own where it has one. A text is **refused** when any class matches, and the verdict
  turns on the most probable class that did.
- Class ids are only keys for matching answers. TypeSafe does not show them to the model.
- An answer that leaves a class out, or gives it anything but a probability from 0 to 1, is an
  error and never a pass. Anything moderating with this should hold a text it cannot get a verdict
  on, not let it through.

## The default policy

`policy/default.json`, built in as `classifier.Default()`:

| class | refuses | line |
|---|---|---|
| `violence` | harm to people | 0.5 |
| `weapons` | help with weapons | 0.5 |
| `rioting` | organising violent unrest | 0.5 |
| `cyberattack` | an attack on computer systems | 0.5 |
| `sexual` | sexual content | 0.5 |
| `targeting` | targeting a private person | 0.5 |
| `hate` | hate against a group | 0.5 |
| `crime` | help committing a crime | 0.7 |
| `tracking` | covertly tracking a person | 0.5 |
| `jailbreak` | an attempt to override an AI's rules | 0.5 |

It was tuned against `jev-1.13.0` on 2026-10-07. At these lines it passed all 69 prompts in a set
of harmless test prompts and refused all 70 in a set of harmful ones, and a re-run with this tool
on 2026-10-26 gave the same result. Crime needs the higher line because harmless explanations of
how a crime works score high on it. Of a list of 1,711 single words and short phrases, it refused
660. Bare words are hard for any moderation model, and real input usually arrives as sentences.

The tuning sets are not in this repository, because they hold what the policy refuses. `testdata/`
has a small harmless pair instead, as a smoke test and a worked example:

```sh
jev-classify -key-file ~/typesafe.key -expect passed  testdata/passes.txt   > /dev/null
jev-classify -key-file ~/typesafe.key -expect refused testdata/refused.txt  > /dev/null
```

`passes.txt` is 22 ordinary prompts, several of them ones a careless policy would catch (explaining
how a lock works, how phishing looks, how encryption works). The highest score any of them got was
0.13 on 2026-10-26. `refused.txt` is 5 attempts to override an AI's rules, the one class that can be
exercised without writing anything harmful; the lowest score was 0.98. Rewording a question changes
its scores, so re-run your own sets after any edit to one (see [Tuning](#tuning)).

**Cost:** each text takes about 1,170 input tokens with all ten questions. On 2026-10-07,
`jev-1.13.0` cost $0.042 per million input tokens, which is about 5 cents per thousand texts.

## Library

```go
import classifier "github.com/DataAlchemy/jev-prompt-classifier"

c := classifier.Classifier{
	Policy: classifier.Default(),
	Client: &classifier.Client{Key: key},
}
v, _, err := c.Classify(ctx, text)
if err != nil {
	// No verdict. Hold the text; do not pass it.
}
if v.Refused() {
	fmt.Println("refused as", v.Top().Class)
}
```

- `Client.Ask` makes one attempt per call. An `*classifier.Error` with `Retryable` set (a rate
  limit, a fault on TypeSafe's side, or a call that never completed) is worth retrying after a
  pause. The retry policy is up to the caller.
- `Policy.Decide` reads answers you already have. It makes no call.
- `Policy.Digest` identifies the model and questions an answer came from. Thresholds are left out
  of it, so answers kept under a digest can be re-read against new lines.
- `Parse` ignores fields it does not know, so one policy file can also carry what its caller adds
  to each class, such as the penalty a match carries.

## Command line

```sh
go build ./cmd/jev-classify
./jev-classify -key-file ~/typesafe.key -scores scores.json prompts.txt > prompts.scored.tsv
```

It writes one tab-separated row per non-blank line, in the file's order:

```
line  verdict  class  score  line_for_class  second_class  second_score  text
```

A summary goes to standard error.

| flag | |
|---|---|
| `-policy file` | the policy; the built-in one by default |
| `-key-file file` | TypeSafe's API key; defaults to `$TYPESAFE_KEY_FILE`, else `$TYPESAFE_API_KEY` holds the key |
| `-scores file` | keep every answer, and never ask about a kept line again |
| `-borderline p` | label a passed line whose top class scored at least `p` as `borderline`, for review |
| `-expect passed\|refused` | list the lines that came out otherwise, and exit 1 if there are any |
| `-parallel n` | calls in flight at once (4) |
| `-attempts n` | tries per line while TypeSafe is busy or failing (5) |
| `-base-url url` | TypeSafe's API (`https://api.typesafe.ai`) |

Exit status is 0 on success, 1 when `-expect` found lines that came out otherwise, and 2 on a
usage error or a line that could not be answered.

### Tuning

1. Score a list, keeping the answers:
   `jev-classify -scores s.json -borderline 0.4 words.txt > words.tsv`.
2. Read the borderline and refused rows.
3. Move a line in a copy of the policy, then run again with the same `-scores`. Kept answers are
   re-read, so this makes no new calls.
4. After rewording a question, run the labelled sets again. The changed digest means those lines
   are asked again:

   ```sh
   jev-classify -scores s.json -expect passed good.txt > /dev/null
   jev-classify -scores s.json -expect refused bad.txt > /dev/null
   ```

The `-scores` file holds digests of the texts, never the texts. The scored TSV does hold them, so
treat it like the list it came from.

## Keys

The key is read from a file (keep it mode 600) or from `$TYPESAFE_API_KEY`. It is sent only in the
`Authorization` header of calls to TypeSafe. No error carries it, and the tool never prints it.

## Development

```sh
go test -race ./...
```

The tests run against a fake TypeSafe with harmless text and never make a real call.

## Licence

Apache License 2.0. See [LICENSE](LICENSE).
