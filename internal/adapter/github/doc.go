// Package github is the reconciler's GitHub adapter (M6-E3, OQ-5, OQ-S4): it reads the state of pull requests
// with a GitHub App installation token, one installation per organization (GHES: one app per host).
//
// It is a thin client over net/http. Authentication is an App JWT (RS256, ten minutes at most) exchanged for an
// installation token, cached until a minute before it expires. Pull request reads send If-None-Match, and a 304
// answers with the last state seen, which GitHub does not count against the rate limit. Each installation is also
// held under its own request budget per hour (default 4,500 of the 5,000 GitHub allows), counting every call, so
// no mix of repositories can exhaust it: a poll over the budget is told to back off until the oldest call leaves
// the hour.
//
// A rate limit (403 or 429 with the limit used up, a Retry-After, a secondary limit) and a server error (5xx)
// block the installation until the reset time, Retry-After or an exponential delay (1 to 30 minutes); the
// caller keeps the last known state meanwhile. The private keys come from the file the apps file names, mounted
// from a Kubernetes Secret that the External Secrets Operator syncs from Secrets Manager; they are never in the
// database and never logged, and neither is a token.
package github
