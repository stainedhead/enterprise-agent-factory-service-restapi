# Implementation notes

- Annotations are pointer booleans because the MCP defaults for absent hints are not all false (destructive and
  open-world default to true). A read-only tool is never destructive.
- Within one policy level the most specific rule wins (exact name > glob > annotation class), so a team can block
  `delete_*` and allow `delete_branch`. Across levels a block always wins.
- Annotation-class rules apply only when the server's annotations are trusted after review (PRD §7.3).
- An unknown approval policy or tier fails closed to security approval.
- `Tokenize` strips a plural `s` only from words longer than three characters, so `bus` stays `bus`.
- Canonicalisation is checked against the RFC 8785 examples (§3.2.2 values, §3.2.3 UTF-16 key order).
