package channel

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	redactedText = "[redacted]"
	// scrubInputLimit bounds the work on the request path: only the start
	// of an upstream error body can reach a 200-character snippet.
	scrubInputLimit = 1024
)

// credentialPattern matches what is treated as a credential: a token after
// "Bearer", an sk-/sk_ style key (masked forms with '*' included), and any
// run of 32 or more base64, base64url or hex characters.
var credentialPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s"'<>,;()\[\]{}]+|\bsk[-_][a-z0-9_*\-]{4,}|[a-z0-9+/=_\-]{32,}`)

// scrubText prepares untrusted upstream text for display: credential-like
// tokens become "[redacted]", every run of whitespace or control characters
// (newlines included) becomes one space, and at most limit characters
// remain. The result never shares memory with s.
func scrubText(s string, limit int) string {
	if len(s) > scrubInputLimit {
		s = cutBeforeToken(s, scrubInputLimit)
	}
	// ReplaceAllStringFunc always returns a fresh copy.
	s = credentialPattern.ReplaceAllStringFunc(s, redactCredential)
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	return truncateRunes(strings.ToValidUTF8(s, "�"), limit)
}

// redactCredential replaces one credentialPattern match. After "Bearer" a
// short plain word ("Bearer token", "Bearer auth") is prose, not a token.
func redactCredential(match string) string {
	const scheme = "bearer"
	if len(match) > len(scheme) && strings.EqualFold(match[:len(scheme)], scheme) {
		rest := match[len(scheme):]
		if token := strings.TrimLeftFunc(rest, unicode.IsSpace); len(token) < len(rest) {
			if isPlainWord(token) {
				return match
			}
			return match[:len(match)-len(token)] + redactedText
		}
	}
	return redactedText
}

func isPlainWord(token string) bool {
	if len(token) >= 16 {
		return false
	}
	for i := 0; i < len(token); i++ {
		if c := token[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// cutBeforeToken shortens s to at most n bytes without splitting a
// character, and drops a token the cut would split: its visible part could
// be a credential prefix too short to be recognized.
func cutBeforeToken(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if !isTokenByte(s[n]) {
		return s[:n]
	}
	for n > 0 && isTokenByte(s[n-1]) {
		n--
	}
	return s[:n]
}

func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("+/=_-*.~", c) >= 0
}

// truncateRunes keeps the first limit characters of s.
func truncateRunes(s string, limit int) string {
	count := 0
	for i := range s {
		if count == limit {
			return s[:i]
		}
		count++
	}
	return s
}
