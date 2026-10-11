package classify

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	reUUID  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reIP    = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	reTime  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:z|[+-]\d{2}:?\d{2})?`)
	reHex   = regexp.MustCompile(`\b(?:0x)?[0-9a-f]{8,}\b`)
	reNum   = regexp.MustCompile(`\d+(?:\.\d+)?`)
	reSpace = regexp.MustCompile(`\s+`)
)

// Pattern reduces a log message to a template by replacing variable parts
// (timestamps, UUIDs, IPs, hex ids, numbers) with placeholders, so that
// repeats of the same problem share a pattern.
func Pattern(msg string) string {
	if len(msg) > 400 {
		msg = msg[:400]
	}
	s := strings.ToLower(msg)
	s = reTime.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reIP.ReplaceAllString(s, "<ip>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reNum.ReplaceAllString(s, "<n>")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Fingerprint identifies "the same problem from the same source". It drives
// grouping in the UI, notification throttling and the AI result cache.
func Fingerprint(sourceID, msg string) string {
	sum := sha1.Sum([]byte(sourceID + "\x00" + Pattern(msg)))
	return hex.EncodeToString(sum[:8])
}
