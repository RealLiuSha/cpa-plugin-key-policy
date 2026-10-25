// Package jsonscan crosses JSON values without decoding them. A string is
// crossed with IndexByte, so megabytes of base64 or conversation history cost
// a memory scan, not a decode. It checks only what crossing needs; a value
// malformed in other ways may be crossed, and whoever decodes it finds out.
package jsonscan

import (
	"bytes"
	"errors"
)

// ErrMalformed reports JSON that cannot be crossed.
var ErrMalformed = errors.New("malformed JSON")

// Members calls visit with each member of the object that raw holds: its key,
// unquoted with any escapes kept, and the span of its value, until visit
// returns false.
func Members(raw []byte, visit func(key []byte, start, end int) bool) error {
	i := SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return ErrMalformed
	}
	i = SkipSpace(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return nil
	}
	for {
		// A key must follow, also after a comma.
		keyEnd, err := SkipString(raw, i)
		if err != nil {
			return err
		}
		key := raw[i+1 : keyEnd-1]
		i = SkipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return ErrMalformed
		}
		start := SkipSpace(raw, i+1)
		end, err := SkipValue(raw, start)
		if err != nil {
			return err
		}
		if !visit(key, start, end) {
			return nil
		}
		i = SkipSpace(raw, end)
		if i >= len(raw) {
			return ErrMalformed
		}
		switch raw[i] {
		case ',':
			i = SkipSpace(raw, i+1)
		case '}':
			return nil
		default:
			return ErrMalformed
		}
	}
}

// Elements calls visit with the span of each element of the array that raw
// holds, until visit returns false.
func Elements(raw []byte, visit func(start, end int) bool) error {
	i := SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return ErrMalformed
	}
	i = SkipSpace(raw, i+1)
	if i < len(raw) && raw[i] == ']' {
		return nil
	}
	for {
		end, err := SkipValue(raw, i)
		if err != nil {
			return err
		}
		if !visit(i, end) {
			return nil
		}
		i = SkipSpace(raw, end)
		if i >= len(raw) {
			return ErrMalformed
		}
		switch raw[i] {
		case ',':
			i = SkipSpace(raw, i+1)
		case ']':
			return nil
		default:
			return ErrMalformed
		}
	}
}

// SkipSpace returns the index of the first non-space byte at or after i.
func SkipSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// SkipString returns the index just past the string literal starting at i.
// A string is crossed with IndexByte: once to the next quote, then from escape
// to escape up to that quote.
func SkipString(raw []byte, i int) (int, error) {
	if i >= len(raw) || raw[i] != '"' {
		return 0, ErrMalformed
	}
	j := i + 1
	for {
		quote := bytes.IndexByte(raw[j:], '"')
		if quote < 0 {
			return 0, ErrMalformed
		}
		end := j + quote
		// Step over the escapes before this quote without searching for it
		// again: searching after every escape is quadratic in a client string
		// full of escapes (a header of backslashes or of '<', which the host's
		// json.Marshal writes as \u003c), and those come before a host request's Body.
		for {
			backslash := bytes.IndexByte(raw[j:end], '\\')
			if backslash < 0 {
				return end + 1, nil
			}
			j += backslash + 2
			if j > end {
				break // the quote at end was escaped
			}
		}
	}
}

// SkipValue returns the index just past the JSON value starting at i.
func SkipValue(raw []byte, i int) (int, error) {
	if i >= len(raw) {
		return 0, ErrMalformed
	}
	switch raw[i] {
	case '"':
		return SkipString(raw, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case '"':
				end, err := SkipString(raw, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, ErrMalformed
	default:
		j := i
		for j < len(raw) {
			switch raw[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				if j == i {
					return 0, ErrMalformed
				}
				return j, nil
			}
			j++
		}
		return 0, ErrMalformed
	}
}
