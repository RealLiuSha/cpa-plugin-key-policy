package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// Host requests for authentication, routing and request interception carry
// the whole client body as one base64 string member named Body, which can be
// tens of megabytes. encoding/json scans every byte of that string even when
// the target struct has no Body field, so these requests are split first:
// the Body member is skipped with one IndexByte (base64 has no quote or
// backslash) and only the remaining small object is decoded.
//
// Every slice returned here may point into the request buffer, which the
// host owns and frees when the call returns; callers copy what they keep.

var errMalformedEnvelope = errors.New("malformed host request")

// decodeSkippingBody decodes raw into v without the top-level Body member and
// returns that member's raw JSON value (a string literal or null, nil when
// absent).
func decodeSkippingBody(raw []byte, v any) (body []byte, err error) {
	rest, body, err := splitBody(raw)
	if err != nil {
		return nil, err
	}
	return body, json.Unmarshal(rest, v)
}

// splitBody returns raw without its top-level Body member, and that member's
// value. rest is raw itself when there is no Body member and a copy
// otherwise; body always points into raw. json.Unmarshal copies everything it
// decodes, so only a custom UnmarshalJSON that keeps its input could hold on
// to the host buffer.
func splitBody(raw []byte) (rest, body []byte, err error) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, nil, errMalformedEnvelope
	}
	i++
	for {
		i = skipSpace(raw, i)
		if i >= len(raw) {
			return nil, nil, errMalformedEnvelope
		}
		if raw[i] == '}' {
			// No Body member: the whole request is small enough to decode.
			return raw, nil, nil
		}
		memberStart := i
		keyEnd, err := skipString(raw, i)
		if err != nil {
			return nil, nil, err
		}
		key := raw[i+1 : keyEnd-1]
		i = skipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, nil, errMalformedEnvelope
		}
		valueStart := skipSpace(raw, i+1)
		valueEnd, err := skipValue(raw, valueStart)
		if err != nil {
			return nil, nil, err
		}
		next := skipSpace(raw, valueEnd)
		if next >= len(raw) {
			return nil, nil, errMalformedEnvelope
		}
		if string(key) == "Body" {
			body = raw[valueStart:valueEnd]
			switch raw[next] {
			case ',':
				rest = make([]byte, 0, memberStart+len(raw)-next)
				rest = append(rest, raw[:memberStart]...)
				rest = append(rest, raw[next+1:]...)
			case '}':
				head := bytes.TrimRight(raw[:memberStart], " \t\r\n")
				head = bytes.TrimSuffix(head, []byte(","))
				rest = make([]byte, 0, len(head)+len(raw)-next)
				rest = append(rest, head...)
				rest = append(rest, raw[next:]...)
			default:
				return nil, nil, errMalformedEnvelope
			}
			return rest, body, nil
		}
		switch raw[next] {
		case ',':
			i = next + 1
		case '}':
			return raw, nil, nil
		default:
			return nil, nil, errMalformedEnvelope
		}
	}
}

// decodeBodyBytes turns the raw Body value from splitBody into the bytes the
// host encoded. It allocates the decoded copy.
func decodeBodyBytes(value []byte) ([]byte, error) {
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, nil
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return nil, errMalformedEnvelope
	}
	inner := value[1 : len(value)-1]
	if bytes.IndexByte(inner, '\\') >= 0 {
		var out []byte
		err := json.Unmarshal(value, &out)
		return out, err
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(inner)))
	n, err := base64.StdEncoding.Decode(out, inner)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

func skipSpace(raw []byte, i int) int {
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

// skipString returns the index just past the string literal starting at i.
// A string is crossed with IndexByte: once to the next quote, then from escape
// to escape up to that quote.
func skipString(raw []byte, i int) (int, error) {
	if i >= len(raw) || raw[i] != '"' {
		return 0, errMalformedEnvelope
	}
	j := i + 1
	for {
		quote := bytes.IndexByte(raw[j:], '"')
		if quote < 0 {
			return 0, errMalformedEnvelope
		}
		end := j + quote
		// Step over the escapes before this quote without searching for it
		// again: searching after every escape is quadratic in a client string
		// full of escapes (a header of backslashes or of '<', which the host's
		// json.Marshal writes as \u003c), and those come before Body.
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

// skipValue returns the index just past the JSON value starting at i.
func skipValue(raw []byte, i int) (int, error) {
	if i >= len(raw) {
		return 0, errMalformedEnvelope
	}
	switch raw[i] {
	case '"':
		return skipString(raw, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case '"':
				end, err := skipString(raw, j)
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
		return 0, errMalformedEnvelope
	default:
		j := i
		for j < len(raw) {
			switch raw[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				if j == i {
					return 0, errMalformedEnvelope
				}
				return j, nil
			}
			j++
		}
		return 0, errMalformedEnvelope
	}
}
