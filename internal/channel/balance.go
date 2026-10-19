package channel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	balanceTimeout = 15 * time.Second
	// balanceBodyLimit caps how much of a balance response is read.
	balanceBodyLimit    = 1 << 20
	balanceSnippetLimit = 120
	balanceErrorLimit   = 300
)

// balanceReading is the stored outcome of a channel's latest balance query.
type balanceReading struct {
	kind string
	at   time.Time
	// known says remaining and unit hold a successful reading from kind,
	// possibly older than at.
	known     bool
	remaining float64
	unit      string
	failure   string
}

func (b balanceReading) state(threshold float64, loc *time.Location) BalanceState {
	return BalanceState{
		Remaining: b.remaining,
		Unit:      b.unit,
		At:        b.at.In(loc),
		Error:     b.failure,
		Low:       b.known && threshold > 0 && b.remaining < threshold,
	}
}

// withBalance attaches the channel's balance reading when it came from the
// source the metadata names now.
func (r *Registry) withBalance(view View, meta Meta) View {
	if meta.Balance.Kind == "" {
		return view
	}
	r.balanceMu.Lock()
	reading, ok := r.balances[view.AuthIndex]
	r.balanceMu.Unlock()
	if ok && reading.kind == meta.Balance.Kind {
		state := reading.state(meta.Balance.ThresholdUSD, r.loc)
		view.Balance = &state
	}
	return view
}

// RefreshBalance queries one channel's balance now and stores the reading.
// A query that ran returns its state and a nil error even when it failed:
// the failure, including a credential missing from CPA's config, is the
// state's Error, which never contains the key. The error is non-nil only
// when nothing was queried: a malformed auth_index (ErrInvalid), a channel
// without a balance source (ErrNoBalanceSource), or ctx ending during the
// query (nothing is stored then).
func (r *Registry) RefreshBalance(ctx context.Context, authIndex string) (BalanceState, error) {
	index, err := normalizeAuthIndex(authIndex)
	if err != nil {
		return BalanceState{}, err
	}
	meta, ok := r.metaFor(index)
	if !ok || meta.Balance.Kind == "" {
		return BalanceState{}, fmt.Errorf("%w: %s", ErrNoBalanceSource, index)
	}
	return r.refreshBalance(ctx, meta)
}

// RefreshBalances queries every channel that has a balance source, one at a
// time, until ctx ends; the plugin calls it from its background loop.
func (r *Registry) RefreshBalances(ctx context.Context) {
	metas := r.metaSnapshot()
	pending := make([]Meta, 0, len(metas))
	for _, meta := range metas {
		if meta.Balance.Kind != "" {
			pending = append(pending, meta)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].AuthIndex < pending[j].AuthIndex })
	for _, meta := range pending {
		if ctx.Err() != nil {
			return
		}
		_, _ = r.refreshBalance(ctx, meta)
	}
}

func (r *Registry) refreshBalance(ctx context.Context, meta Meta) (BalanceState, error) {
	remaining, unit, err := r.queryBalance(ctx, meta.AuthIndex, meta.Balance.Kind)
	if err != nil && ctx.Err() != nil {
		return BalanceState{}, ctx.Err()
	}
	return r.storeBalance(meta, remaining, unit, err), nil
}

func (r *Registry) storeBalance(meta Meta, remaining float64, unit string, failure error) BalanceState {
	index, threshold := meta.AuthIndex, meta.Balance.ThresholdUSD
	if current, ok := r.metaFor(index); ok {
		threshold = current.Balance.ThresholdUSD
	}
	next := balanceReading{kind: meta.Balance.Kind, at: r.now(), known: true, remaining: remaining, unit: unit}
	r.balanceMu.Lock()
	previous, had := r.balances[index]
	if !had || previous.kind != next.kind {
		previous = balanceReading{}
	}
	if failure != nil {
		next.known, next.remaining, next.unit = previous.known, previous.remaining, previous.unit
		next.failure = failure.Error()
	}
	r.balances[index] = next
	r.balanceMu.Unlock()

	state := next.state(threshold, r.loc)
	switch {
	case next.failure != "" && next.failure != previous.failure:
		r.logf("warn", fmt.Sprintf("cpa-key-policy: channel %s balance query failed: %s", index, next.failure))
	case next.failure == "" && previous.failure != "":
		r.logf("info", fmt.Sprintf("cpa-key-policy: channel %s balance query succeeded again", index))
	}
	if state.Low && !previous.state(threshold, r.loc).Low {
		amount := fmt.Sprintf("%g", state.Remaining)
		if state.Unit != "" {
			amount += " " + state.Unit
		}
		r.logf("warn", fmt.Sprintf("cpa-key-policy: channel %s balance %s is below the %g threshold", index, amount, threshold))
	}
	return state
}

// queryBalance reads the balance with the credential CPA has for index. Its
// error text is display-ready and never contains the key.
func (r *Registry) queryBalance(ctx context.Context, index, kind string) (float64, string, error) {
	var credential Credential
	found := false
	if r.credentials != nil {
		credential, found = r.credentials(index)
	}
	if !found {
		return 0, "", errors.New("credential not found in CPA's config")
	}
	key := strings.TrimSpace(credential.APIKey)
	if key == "" {
		return 0, "", errors.New("the credential in CPA's config has no API key")
	}
	origin := balanceOrigin(credential.BaseURL)
	if origin == "" {
		return 0, "", errors.New("the credential in CPA's config has no base URL")
	}
	ctx = context.WithValue(ctx, proxyKey{}, credential.ProxyURL)
	var remaining float64
	var unit string
	var err error
	switch kind {
	case BalanceSub2API:
		remaining, unit, err = r.querySub2API(ctx, origin, key)
	case BalanceOpenAIBilling:
		remaining, unit, err = r.queryOpenAIBilling(ctx, origin, key)
	default:
		err = fmt.Errorf("unknown balance source %q", kind)
	}
	if err != nil {
		return 0, "", errors.New(truncateRunes(redactSecret(err.Error(), key), balanceErrorLimit))
	}
	return remaining, unit, nil
}

// balanceOrigin strips a trailing "/" and "/v1" from a credential's base
// URL; the balance endpoints live under the site root.
func balanceOrigin(baseURL string) string {
	origin := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return strings.TrimRight(strings.TrimSuffix(origin, "/v1"), "/")
}

type sub2APIUsage struct {
	Remaining    *float64 `json:"remaining"`
	Balance      *float64 `json:"balance"`
	Unit         string   `json:"unit"`
	Mode         string   `json:"mode"`
	IsValid      *bool    `json:"isValid"`
	Subscription *struct {
		DailyLimitUSD   *float64 `json:"daily_limit_usd"`
		WeeklyLimitUSD  *float64 `json:"weekly_limit_usd"`
		MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	} `json:"subscription"`
	Error json.RawMessage `json:"error"`
}

// querySub2API reads Sub2API's GET /v1/usage. remaining is the quota left
// (quota_limited mode), the subscription's smallest remaining limit or the
// wallet balance (unrestricted mode); a subscription without any limit
// reports -1 and has no finite balance.
func (r *Registry) querySub2API(ctx context.Context, origin, key string) (float64, string, error) {
	const path = "/v1/usage"
	var usage sub2APIUsage
	status, err := r.getJSON(ctx, origin, path, key, &usage)
	if err != nil {
		return 0, "", err
	}
	fail := func(detail string) (float64, string, error) {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: %s", path, status, detail)
	}
	if message := upstreamError(usage.Error, key); message != "" {
		return fail(message)
	}
	if usage.IsValid != nil && !*usage.IsValid {
		return fail("Sub2API reports the key as invalid")
	}
	remaining := usage.Remaining
	if remaining == nil {
		remaining = usage.Balance
	}
	if remaining == nil {
		return fail(fmt.Sprintf("the response has no remaining balance (mode %q)", usage.Mode))
	}
	if s := usage.Subscription; s != nil && s.DailyLimitUSD == nil && s.WeeklyLimitUSD == nil && s.MonthlyLimitUSD == nil {
		return fail("the subscription has no limit, so there is no finite balance")
	}
	unit := strings.TrimSpace(usage.Unit)
	if unit == "" {
		unit = "USD"
	}
	return *remaining, unit, nil
}

// unlimitedHardLimitUSD is the hard_limit_usd new-api and one-api report
// for a token with unlimited quota; such a token cannot see the balance of
// the account behind it. new-api sets exactly this value; any other value is
// the token's quota in the site's display unit, which on a site that shows
// raw quota can be larger.
const unlimitedHardLimitUSD = 100000000

// queryOpenAIBilling reads the OpenAI-style billing pair: remaining =
// hard_limit_usd - total_usage/100 (total_usage is in hundredths), in the
// site's display unit (USD, CNY or raw quota), which the answer does not
// name, so the unit is empty. The usage request carries the date range
// new-api's own balance client sends; new-api and one-api ignore it, other
// relays require it.
func (r *Registry) queryOpenAIBilling(ctx context.Context, origin, key string) (float64, string, error) {
	const subscriptionPath, usagePath = "/v1/dashboard/billing/subscription", "/v1/dashboard/billing/usage"
	var subscription struct {
		HardLimitUSD     *float64        `json:"hard_limit_usd"`
		HasPaymentMethod bool            `json:"has_payment_method"`
		Error            json.RawMessage `json:"error"`
	}
	status, err := r.getJSON(ctx, origin, subscriptionPath, key, &subscription)
	if err != nil {
		return 0, "", err
	}
	if message := upstreamError(subscription.Error, key); message != "" {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: %s", subscriptionPath, status, message)
	}
	if subscription.HardLimitUSD == nil {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: the response has no hard_limit_usd", subscriptionPath, status)
	}
	if *subscription.HardLimitUSD == unlimitedHardLimitUSD {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: the key has unlimited quota on this site, so the account balance cannot be read with it", subscriptionPath, status)
	}
	today := r.now().In(r.loc)
	start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, r.loc)
	if !subscription.HasPaymentMethod {
		start = today.AddDate(0, 0, -100)
	}
	var usage struct {
		TotalUsage *float64        `json:"total_usage"`
		Error      json.RawMessage `json:"error"`
	}
	query := "?start_date=" + start.Format(time.DateOnly) + "&end_date=" + today.Format(time.DateOnly)
	if status, err = r.getJSON(ctx, origin, usagePath+query, key, &usage); err != nil {
		return 0, "", err
	}
	if message := upstreamError(usage.Error, key); message != "" {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: %s", usagePath, status, message)
	}
	if usage.TotalUsage == nil {
		return 0, "", fmt.Errorf("GET %s: HTTP %d: the response has no total_usage", usagePath, status)
	}
	return *subscription.HardLimitUSD - *usage.TotalUsage/100, "", nil
}

// getJSON sends an authenticated GET and decodes a 2xx JSON answer into out.
// Its errors name the path (without query), the status and at most 120
// scrubbed characters of the body.
func (r *Registry) getJSON(ctx context.Context, origin, pathAndQuery, key string, out any) (int, error) {
	path, _, _ := strings.Cut(pathAndQuery, "?")
	status, body, err := r.httpDo(ctx, http.MethodGet, origin+pathAndQuery, map[string]string{
		"Authorization": "Bearer " + key,
		"Accept":        "application/json",
	})
	if err != nil {
		return 0, fmt.Errorf("GET %s: %v", path, err)
	}
	if status < 200 || status > 299 {
		return status, fmt.Errorf("GET %s: HTTP %d%s", path, status, bodySuffix(body, key))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return status, fmt.Errorf("GET %s: HTTP %d: the response is not the expected JSON%s", path, status, bodySuffix(body, key))
	}
	return status, nil
}

func bodySuffix(body []byte, key string) string {
	if len(body) > scrubInputLimit+len(key) {
		body = body[:scrubInputLimit+len(key)]
	}
	if text := scrubText(redactSecret(string(body), key), balanceSnippetLimit); text != "" {
		return ": " + text
	}
	return ""
}

// upstreamError describes an "error" member of a successful response, or
// returns "" when there is none.
func upstreamError(raw json.RawMessage, key string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	text := string(raw)
	var object struct {
		Message string `json:"message"`
	}
	var plain string
	switch {
	case json.Unmarshal(raw, &object) == nil && object.Message != "":
		text = object.Message
	case json.Unmarshal(raw, &plain) == nil && plain != "":
		text = plain
	}
	return "upstream error: " + scrubText(redactSecret(text, key), balanceSnippetLimit)
}

// proxyKey carries the credential's proxy-url in a balance request's context.
type proxyKey struct{}

// balanceProxy picks the proxy CPA would use for the credential, as CPA's
// proxyutil.Parse reads a proxy-url: empty inherits the environment's proxy,
// "direct" or "none" means none, and otherwise it is an http, https, socks5
// or socks5h URL. The errors never quote the value, which may hold a
// password.
func balanceProxy(request *http.Request) (*url.URL, error) {
	raw, _ := request.Context().Value(proxyKey{}).(string)
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return http.ProxyFromEnvironment(request)
	case strings.EqualFold(raw, "direct"), strings.EqualFold(raw, "none"):
		return nil, nil
	}
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Scheme == "" || proxy.Host == "" {
		return nil, errors.New("the proxy-url CPA's config gives this credential is not a valid URL")
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
		return proxy, nil
	}
	return nil, errors.New("the proxy-url CPA's config gives this credential has an unsupported scheme")
}

// newHTTPDo is the default balance client: 15 s per request, the
// credential's proxy as CPA uses it, at most 1 MiB of each response body.
// The transport keeps connections per proxy. It speaks HTTP/1.1 only, to the
// proxy as CPA's proxyutil does for an https proxy (a proxy that picks h2
// cannot take the HTTP/1.1 CONNECT that follows) and so also to the site.
func newHTTPDo() func(ctx context.Context, method, url string, headers map[string]string) (int, []byte, error) {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.Proxy = balanceProxy
	// Both are needed: the clone inherits DefaultTransport's ALPN list
	// [h2 http/1.1], which an empty TLSNextProto alone leaves in place.
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	client := &http.Client{Timeout: balanceTimeout, Transport: transport}
	return func(ctx context.Context, method, url string, headers map[string]string) (int, []byte, error) {
		request, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return 0, nil, err
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, balanceBodyLimit))
		if err != nil {
			return response.StatusCode, nil, err
		}
		return response.StatusCode, body, nil
	}
}
