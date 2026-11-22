// Package condmsg sanitises the text of a status condition. Conditions are read by team viewers through
// the API, so the raw error of an AWS or Kubernetes client (role ARNs, account ids, tokens, unbounded
// length) must not be stored in them; the full error goes to the reconciler log instead (FR-R13).
package condmsg

import (
	"regexp"
	"sync/atomic"
	"unicode/utf8"
)

// MaxBytes is the longest condition message.
const MaxBytes = 512

var (
	arnRe     = regexp.MustCompile(`arn:aws[a-z-]*:[A-Za-z0-9-]*:[A-Za-z0-9-]*:[0-9]*:[^\s"',;)\]]*`)
	accountRe = regexp.MustCompile(`\b[0-9]{12}\b`)
)

var scrubber atomic.Pointer[func(string) string]

// SetScrubber injects the secret scrubber applied before the ARN and account id removal
// (cmd/afs-reconciler passes observability.Scrub; usecase packages do not import adapters). nil removes it.
func SetScrubber(f func(string) string) {
	if f == nil {
		scrubber.Store(nil)
		return
	}
	scrubber.Store(&f)
}

// Sanitize applies the injected scrubber, replaces ARNs with <arn> and 12-digit account ids with
// <account-id>, and cuts the result to MaxBytes on a rune boundary.
func Sanitize(msg string) string {
	if f := scrubber.Load(); f != nil {
		msg = (*f)(msg)
	}
	msg = arnRe.ReplaceAllString(msg, "<arn>")
	msg = accountRe.ReplaceAllString(msg, "<account-id>")
	if len(msg) <= MaxBytes {
		return msg
	}
	cut := MaxBytes
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
