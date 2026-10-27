# Contributing

Pull requests are welcome. The owner reviews and merges every one: nothing lands on `main` without
an approving review, and only the owner can merge.

## Before you open one

- Open an issue first for anything bigger than a small fix, so you don't write something that
  won't be taken.
- Run the checks CI runs: `gofmt -l .` (it should print nothing), `go vet ./...` and
  `go test -race ./...`. The tests use a fake TypeSafe server and make no real call, so you need no key.
- Keep the library to the standard library. A new dependency needs a reason in the pull request.

## Changing the policy

`policy/default.json` is the policy this repository ships. Each class is one yes-or-no question put
to jev, and jev answers the question that was written, not the one that was meant. A reworded
question or a moved line changes what gets refused, so a pull request that touches the policy must say
what it did to a labelled set of your own: how many harmless lines it wrongly refuses and how many
lines that should be refused it now misses, before and after. `jev-classify -expect` prints both.
Don't attach your sets.

## No harmful content

Never put lewd, hateful, violent or otherwise harmful text in a pull request, an issue, a test or
an example: not as a test case, not "for coverage", and not partly redacted. The tests in this
repository use harmless text only, and `testdata/refused.txt` is limited to attempts to override an AI's
rules for that reason. A pull request that adds harmful text will be closed.

## Keys

Never put a TypeSafe key, or any other secret, in a pull request or an issue. If you did by
accident, revoke the key first, then tell us.

## Licence

By contributing you agree your contribution is licensed under the Apache License 2.0, the
repository's licence.

## How `main` is protected

The rules are in `.github/rulesets/main.json`, which a repository admin imports under Settings, Rules,
Rulesets: a pull request is required, with one approving review that is dismissed when new commits
are pushed, the `check` job must pass, history stays linear, and `main` cannot be force-pushed or
deleted. Only a repository admin can bypass them, and only when merging a pull request.
