package homeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls the CLIProxyAPIHome management API. Every Home in a cluster
// reads and writes the same database, so any reachable Home is authoritative;
// the client tries the configured Homes in order and moves on only when a
// Home cannot be reached at all.
type Client struct {
	bases []string
	key   string
	http  *http.Client
}

// HomeError is a response Home produced; transport failures are plain errors.
type HomeError struct {
	Status  int
	Code    string
	Message string
}

func (e *HomeError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("home %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("home %d %s", e.Status, e.Code)
}

func NewClient(bases []string, key string, timeout time.Duration) *Client {
	trimmed := make([]string, 0, len(bases))
	for _, base := range bases {
		if base = strings.TrimRight(strings.TrimSpace(base), "/"); base != "" {
			trimmed = append(trimmed, base)
		}
	}
	return &Client{bases: trimmed, key: key, http: &http.Client{Timeout: timeout}}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = encoded
	}
	target := "/v8/management" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	lastErr := errors.New("no Home configured")
	for _, base := range c.bases {
		req, err := http.NewRequestWithContext(ctx, method, base+target, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s via %s: %w", method, path, base, err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("%s %s: read response: %w", method, path, readErr)
		}
		if resp.StatusCode >= 300 {
			return decodeHomeError(resp.StatusCode, data)
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
		return nil
	}
	return lastErr
}

// decodeHomeError accepts both the flat {"error":"code","message":...} shape of
// cluster handlers and the nested {"error":{"code","message"}} shape.
func decodeHomeError(status int, data []byte) error {
	var flat struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	homeErr := &HomeError{Status: status}
	if json.Unmarshal(data, &flat) == nil {
		homeErr.Message = flat.Message
		switch value := flat.Error.(type) {
		case string:
			homeErr.Code = value
		case map[string]any:
			homeErr.Code, _ = value["code"].(string)
			if message, ok := value["message"].(string); ok && homeErr.Message == "" {
				homeErr.Message = message
			}
		}
	}
	if homeErr.Code == "" {
		homeErr.Code = http.StatusText(status)
	}
	return homeErr
}

// User mirrors the Home user fields this adapter reads.
type User struct {
	ID               uint       `json:"id"`
	Username         string     `json:"username"`
	Credits          float64    `json:"credits"`
	CreditsUnlimited bool       `json:"credits_unlimited"`
	Timezone         string     `json:"timezone"`
	Limit1d          *float64   `json:"limit_1d_credits"`
	Limit7d          *float64   `json:"limit_7d_credits"`
	Limit30d         *float64   `json:"limit_30d_credits"`
	WeekResetDay     int        `json:"week_reset_day"`
	CreatedAt        *time.Time `json:"created_at"`
	UpdatedAt        *time.Time `json:"updated_at"`
}

// UserWrite is a partial update; nil pointers are left unchanged and a
// limit set to Unlimited is sent as JSON null.
type UserWrite struct {
	Username         *string  `json:"username,omitempty"`
	Credits          *float64 `json:"credits,omitempty"`
	CreditsUnlimited *bool    `json:"credits_unlimited,omitempty"`
	Timezone         *string  `json:"timezone,omitempty"`
	Limit1d          *Limit   `json:"limit_1d_credits,omitempty"`
	WindowMode1d     *string  `json:"window_mode_1d,omitempty"`
	Limit7d          *Limit   `json:"limit_7d_credits,omitempty"`
	WindowMode7d     *string  `json:"window_mode_7d,omitempty"`
	WeekResetDay     *int     `json:"week_reset_day,omitempty"`
	WeekResetHour    *int     `json:"week_reset_hour,omitempty"`
	Limit30d         *Limit   `json:"limit_30d_credits,omitempty"`
	WindowMode30d    *string  `json:"window_mode_30d,omitempty"`
}

// Limit is a Home credit limit where zero means "no limit" (JSON null), the
// same meaning cpa-key-policy gives a zero USD limit. Home itself treats 0 as
// "always blocked", so the conversion must happen in exactly one place.
type Limit float64

func (l Limit) MarshalJSON() ([]byte, error) {
	if l <= 0 {
		return []byte("null"), nil
	}
	return json.Marshal(float64(l))
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out struct {
		Users []User `json:"users"`
	}
	err := c.do(ctx, http.MethodGet, "/users", nil, nil, &out)
	return out.Users, err
}

func (c *Client) CreateUser(ctx context.Context, write UserWrite) (User, error) {
	var out struct {
		User User `json:"user"`
	}
	err := c.do(ctx, http.MethodPost, "/users", nil, write, &out)
	return out.User, err
}

func (c *Client) UpdateUser(ctx context.Context, id uint, write UserWrite) (User, error) {
	var out struct {
		User User `json:"user"`
	}
	err := c.do(ctx, http.MethodPatch, "/users/"+strconv.FormatUint(uint64(id), 10), nil, write, &out)
	return out.User, err
}

func (c *Client) DeleteUser(ctx context.Context, id uint) error {
	return c.do(ctx, http.MethodDelete, "/users/"+strconv.FormatUint(uint64(id), 10), nil, nil, nil)
}

// PeriodWindow is one entry of GET /users/:id/period-limits.
type PeriodWindow struct {
	ID          string     `json:"id"`
	Enabled     bool       `json:"enabled"`
	Limit       *float64   `json:"limit"`
	Used        float64    `json:"used"`
	Mode        string     `json:"mode"`
	Active      bool       `json:"active"`
	WindowStart *time.Time `json:"window_start"`
	WindowEnd   *time.Time `json:"window_end"`
	ResetAt     *time.Time `json:"reset_at"`
	UsageEpoch  *time.Time `json:"usage_epoch"`
}

func (c *Client) PeriodLimits(ctx context.Context, id uint) ([]PeriodWindow, error) {
	var out struct {
		Windows []PeriodWindow `json:"windows"`
	}
	err := c.do(ctx, http.MethodGet, "/users/"+strconv.FormatUint(uint64(id), 10)+"/period-limits", nil, nil, &out)
	return out.Windows, err
}

func (c *Client) ResetPeriod(ctx context.Context, id uint, windows []string) error {
	body := map[string]any{"windows": windows, "mode": "counter"}
	return c.do(ctx, http.MethodPost, "/users/"+strconv.FormatUint(uint64(id), 10)+"/period-limits/reset", nil, body, nil)
}

// APIKey is a Home client key; Home stores and returns the plain value.
type APIKey struct {
	ID          uint    `json:"id"`
	Value       string  `json:"api_key"`
	DisplayName *string `json:"display_name"`
	UserID      *uint   `json:"user_id"`
	ModelGroups []uint  `json:"model_groups"`
}

func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var out struct {
		Items []APIKey `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/access/api-keys", nil, nil, &out)
	return out.Items, err
}

func (c *Client) CreateAPIKey(ctx context.Context, value, displayName string, userID uint, groups []uint) error {
	body := map[string]any{"api_key": value, "display_name": displayName, "user_id": userID, "model_groups": groups}
	return c.do(ctx, http.MethodPost, "/access/api-keys", nil, body, nil)
}

// UpdateAPIKey changes the display name and model groups of one key by id.
func (c *Client) UpdateAPIKey(ctx context.Context, id uint, displayName string, groups []uint) error {
	body := map[string]any{"id": id, "display_name": displayName, "model_groups": groups}
	return c.do(ctx, http.MethodPatch, "/access/api-keys", nil, body, nil)
}

func (c *Client) DeleteAPIKey(ctx context.Context, id uint) error {
	return c.do(ctx, http.MethodDelete, "/access/api-keys", url.Values{"id": {strconv.FormatUint(uint64(id), 10)}}, nil, nil)
}

type ModelGroup struct {
	ID       uint   `json:"id"`
	Name     string `json:"group_name"`
	Disabled bool   `json:"disabled"`
}

type ModelGroupDetail struct {
	ID      uint   `json:"id"`
	GroupID uint   `json:"model_group_id"`
	ModelID string `json:"model_id"`
}

func (c *Client) ListModelGroups(ctx context.Context) ([]ModelGroup, []ModelGroupDetail, error) {
	var groups struct {
		Items []ModelGroup `json:"model_groups"`
	}
	if err := c.do(ctx, http.MethodGet, "/model-groups", nil, nil, &groups); err != nil {
		return nil, nil, err
	}
	var details struct {
		Items []ModelGroupDetail `json:"model_group_details"`
	}
	if err := c.do(ctx, http.MethodGet, "/model-group-details", nil, nil, &details); err != nil {
		return nil, nil, err
	}
	return groups.Items, details.Items, nil
}

func (c *Client) CreateModelGroup(ctx context.Context, name string) (ModelGroup, error) {
	var out struct {
		Group ModelGroup `json:"model_group"`
	}
	err := c.do(ctx, http.MethodPost, "/model-groups", nil, map[string]any{"group_name": name}, &out)
	return out.Group, err
}

func (c *Client) CreateModelGroupDetail(ctx context.Context, groupID uint, model string) error {
	body := map[string]any{"model_group_id": groupID, "model_id": model, "channels": []uint{}}
	return c.do(ctx, http.MethodPost, "/model-group-details", nil, body, nil)
}

// ModelPrice is one Home billing rule; prices are USD per million tokens.
type ModelPrice struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	ServiceTier    string  `json:"service_tier"`
	MinInputTokens int64   `json:"min_input_tokens"`
	Input          float64 `json:"input_price_per_million"`
	Output         float64 `json:"output_price_per_million"`
	CacheRead      float64 `json:"cache_read_price_per_million"`
	CacheWrite     float64 `json:"cache_write_price_per_million"`
	Request        float64 `json:"request_price"`
	Enabled        bool    `json:"enabled"`
}

func (c *Client) ListModelPrices(ctx context.Context) ([]ModelPrice, error) {
	var out struct {
		Items []ModelPrice `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/billing/model-prices", nil, nil, &out)
	return out.Items, err
}

// Charge is one billed request.
type Charge struct {
	Model        string    `json:"model"`
	Amount       float64   `json:"amount"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CacheTokens  int64     `json:"cache_tokens"`
	CreatedAt    time.Time `json:"created_at"`
}

// ListCharges returns every charge of a user in [from, to).
func (c *Client) ListCharges(ctx context.Context, userID uint, from, to time.Time) ([]Charge, error) {
	const page = 200
	var all []Charge
	for offset := 0; ; offset += page {
		query := url.Values{
			"user_id": {strconv.FormatUint(uint64(userID), 10)},
			"from":    {from.UTC().Format(time.RFC3339Nano)},
			"to":      {to.UTC().Format(time.RFC3339Nano)},
			"limit":   {strconv.Itoa(page)},
			"offset":  {strconv.Itoa(offset)},
		}
		var out struct {
			Items []Charge `json:"items"`
			Total int      `json:"total"`
		}
		if err := c.do(ctx, http.MethodGet, "/billing/charges", query, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if len(out.Items) == 0 || len(all) >= out.Total {
			return all, nil
		}
	}
}
