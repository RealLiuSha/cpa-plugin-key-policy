package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"

	"cpa-key-policy/internal/jsonscan"
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

var errMalformedEnvelope = jsonscan.ErrMalformed

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
	i := jsonscan.SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, nil, errMalformedEnvelope
	}
	i++
	for {
		i = jsonscan.SkipSpace(raw, i)
		if i >= len(raw) {
			return nil, nil, errMalformedEnvelope
		}
		if raw[i] == '}' {
			// No Body member: the whole request is small enough to decode.
			return raw, nil, nil
		}
		memberStart := i
		keyEnd, err := jsonscan.SkipString(raw, i)
		if err != nil {
			return nil, nil, err
		}
		key := raw[i+1 : keyEnd-1]
		i = jsonscan.SkipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, nil, errMalformedEnvelope
		}
		valueStart := jsonscan.SkipSpace(raw, i+1)
		valueEnd, err := jsonscan.SkipValue(raw, valueStart)
		if err != nil {
			return nil, nil, err
		}
		next := jsonscan.SkipSpace(raw, valueEnd)
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

// scanMembers calls visit with each top-level member of a host request, its
// key and raw value, until visit returns false. A member visit does not
// decode costs what crossing it costs: one IndexByte per string.
func scanMembers(raw []byte, visit func(key, value []byte) bool) error {
	return jsonscan.Members(raw, func(key []byte, start, end int) bool { return visit(key, raw[start:end]) })
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
