# Security

## Reporting a problem

Please report a vulnerability privately, through this repository's **Security** tab ("Report a
vulnerability"). Don't open a public issue for it.

Things worth reporting:

- a way a text can slip past a policy because of how this code reads the model's answers, such as a
  class counted as answered when it wasn't, a probability outside 0 to 1 accepted, or an error that
  turns into a pass;
- anywhere a TypeSafe key, or the text being classified, could end up in an error message, a log
  line or a file;
- a flaw in how `jev-classify` keeps answers (`-scores`), such as a stored text, or an answer
  read back under the wrong policy.

Not a vulnerability here: a particular text that jev scores below a line. That is a question of how
the policy is worded, and an issue or a pull request about it is welcome.

You can expect an acknowledgement within a week. Please give us a reasonable time to fix a problem
before you publish it.

## Supported versions

Only the latest commit on `main`.
