package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// hostIntercept mirrors the field order of pluginapi.RequestInterceptRequest
// plus the wrapper's trailing host_callback_id, as the host encodes it.
type hostIntercept struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Headers        http.Header
	Body           []byte
	Metadata       map[string]any
	HostCallbackID string `json:"host_callback_id"`
}

func TestDecodeSkippingBody(t *testing.T) {
	big := bytes.Repeat([]byte(`{"input":"x"}`), 50000)
	tricky := hostIntercept{
		TraceID: "a1b2c3d4",
		// A header and a metadata key named Body must not be taken for the
		// top-level member.
		Headers:  http.Header{"Body": {`"Body":"x"`}, "Authorization": {"Bearer k"}},
		Body:     []byte(`{"model":"gpt-6-sol"}`),
		Metadata: map[string]any{"request_path": "/v1/responses", "Body": "nested"},
	}
	cases := []struct {
		name     string
		raw      []byte
		wantBody []byte
		wantPath string
		wantErr  bool
	}{
		{name: "body in the middle", raw: mustMarshal(t, hostIntercept{TraceID: "t1", Body: big, Metadata: map[string]any{"request_path": "/v1/responses"}}), wantBody: big, wantPath: "/v1/responses"},
		{name: "nested Body keys", raw: mustMarshal(t, tricky), wantBody: tricky.Body, wantPath: "/v1/responses"},
		{name: "body last", raw: []byte(`{"TraceID":"t2","Metadata":{"request_path":"/v1/images/edits"},"Body":"e30="}`), wantBody: []byte("{}"), wantPath: "/v1/images/edits"},
		{name: "null body", raw: []byte(`{"TraceID":"t3","Body":null,"Metadata":{"request_path":"/p"}}`), wantPath: "/p"},
		{name: "no body", raw: []byte(` {"TraceID":"t4","Metadata":{"request_path":"/q"}} `), wantPath: "/q"},
		{name: "metadata of another type", raw: []byte(`{"TraceID":"t6","Metadata":{"request_path":7,"selected_auth_index":{"a":1}}}`)},
		{name: "escaped body string", raw: []byte(`{"Body":"e30=","Metadata":{"request_path":"/e"}}`), wantBody: []byte("{}"), wantPath: "/e"},
		{name: "truncated", raw: []byte(`{"TraceID":"t5","Body":"e30=`), wantErr: true},
		{name: "not an object", raw: []byte(`["Body"]`), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got RequestInterceptRequest
			value, err := decodeSkippingBody(tc.raw, &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if path := string(got.Metadata.RequestPath); path != tc.wantPath {
				t.Fatalf("request_path = %q, want %q", path, tc.wantPath)
			}
			body, err := decodeBodyBytes(value)
			if err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !bytes.Equal(body, tc.wantBody) {
				t.Fatalf("body = %.40q (%d bytes), want %.40q (%d bytes)", body, len(body), tc.wantBody, len(tc.wantBody))
			}
		})
	}

	var withHeaders RequestInterceptRequest
	if _, err := decodeSkippingBody(mustMarshal(t, tricky), &withHeaders); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(withHeaders.Headers["Body"], ""); got != `"Body":"x"` {
		t.Fatalf("header value changed: %q", got)
	}
}

// hostAuth mirrors the field order of pluginapi.FrontendAuthRequest: the
// client's path, headers and query come before Body.
type hostAuth struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

// Client strings in front of Body are crossed escape by escape: an escaped
// quote does not end them and an escaped backslash does not hide the closing
// quote after it.
func TestDecodeSkippingBodyEscapes(t *testing.T) {
	tricky := []string{`a"`, `a\`, `a\"`, `\\`, `"`, `\`, `\\\"`, `<>&`, "\u2028\u2029", `"Body":"x"`, ""}
	host := hostAuth{Method: "POST", Path: "/v1/" + strings.Join(tricky, "/"), Headers: http.Header{}, Query: url.Values{}, Body: []byte(`{"model":"m"}`)}
	for i, v := range tricky {
		host.Headers["X-"+v] = []string{v, v + "x"}
		host.Query[v+"q"] = []string{v}
		host.Headers["X-Pad"] = append(host.Headers["X-Pad"], strings.Repeat(v, i*7))
	}
	indented, err := json.MarshalIndent(host, "", " \t")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		raw      []byte
		wantBody []byte
	}{
		{name: "host encoding", raw: mustMarshal(t, host), wantBody: host.Body},
		{name: "indented", raw: indented, wantBody: host.Body},
		// Escapes json.Marshal never writes but any JSON encoder may.
		{name: "other escapes", raw: []byte(`{"Method":"POST","Path":"/a\"","Headers":{"X":["\\","\\\"","\u0022\/","\\\\\\\"","\\\\"]},"Query":{"q\\":["\"\\"]},"Body":"e30="}`), wantBody: []byte("{}")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want FrontendAuthRequest
			if err := json.Unmarshal(tc.raw, &want); err != nil {
				t.Fatal(err)
			}
			var got FrontendAuthRequest
			value, err := decodeSkippingBody(tc.raw, &got)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded %+v, want %+v", got, want)
			}
			body, err := decodeBodyBytes(value)
			if err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !bytes.Equal(body, tc.wantBody) {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}

	for _, raw := range []string{
		`{"Path":"\`,
		`{"Path":"x\"`,
		`{"Path":"x\\\"`,
		`{"Path":"x\\\"}`,
		`{"Headers":{"X":["\"]},"Body":"e30="}`,
	} {
		var got FrontendAuthRequest
		if _, err := decodeSkippingBody([]byte(raw), &got); err == nil {
			t.Fatalf("%s: expected an error, got %+v", raw, got)
		}
	}
}

// authenticate runs on every request without a native key, before any key
// check, and the host puts no timeout on plugin calls, so a client header
// full of escapes must be crossed in linear time. The bound is relative to
// json.Unmarshal of the same envelope so it does not depend on the machine:
// the fixed scan is about 2x, a scan that searches for the closing quote again
// after every escape is several hundred times slower at this size.
func TestDecodeSkippingBodyIsLinearInEscapes(t *testing.T) {
	for _, ch := range []string{`\`, `<`} {
		raw := mustMarshal(t, hostAuth{
			Method:  "POST",
			Path:    "/v1/chat/completions",
			Headers: http.Header{"X-Pad": {strings.Repeat(ch, 256<<10)}, "Authorization": {"Bearer nope"}},
			Body:    []byte(`{"model":"m"}`),
		})
		var skip, full time.Duration
		for attempt := range 3 {
			start := time.Now()
			var want FrontendAuthRequest
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			unmarshal := time.Since(start)

			start = time.Now()
			var got FrontendAuthRequest
			if _, err := decodeSkippingBody(raw, &got); err != nil {
				t.Fatal(err)
			}
			split := time.Since(start)
			if got.Headers.Get("X-Pad") != want.Headers.Get("X-Pad") {
				t.Fatalf("%q header changed", ch)
			}
			if attempt == 0 {
				skip, full = split, unmarshal
			}
			skip, full = min(skip, split), min(full, unmarshal)
		}
		if skip > 10*full {
			t.Fatalf("header of %d %q (%d-byte envelope): decodeSkippingBody %v, json.Unmarshal %v", 256<<10, ch, len(raw), skip, full)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
