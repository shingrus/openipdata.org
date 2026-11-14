package rdnsrules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	openRouterBase   = "https://openrouter.ai/api/v1"
	requestTimeout   = 180 * time.Second
	maxOutputTokens  = 16000
	maxAttempts      = 4 // the first try and 3 retries
	maxRetryAfter    = 2 * time.Minute
	defaultBackoff   = 5 * time.Second
	maxResponseBytes = 8 << 20
)

// ErrInvalidKey is the fixed error of a rejected key (HTTP 401); it names nothing else.
var ErrInvalidKey = errors.New("OpenRouter rejected the API key (HTTP 401)")

// newOpenRouterHTTP dials IPv4 only: on the VM the IPv6 egress stalls (a connection can open
// and then hang, which Go's dual-stack fallback does not catch).
func newOpenRouterHTTP(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: timeout}
}

// What went wrong with a request.
type apiKind int

const (
	apiStopCredit apiKind = iota + 1 // the key's limit or a budget is used up: stop asking
	apiAuth                          // invalid key
	apiBlocked                       // moderation or permissions: this domain only
	apiTemporary                     // 429, 5xx, timeouts, after the retries
	apiOther                         // anything else (bad request, empty answer, ...)
)

// apiError never holds the key, a header or a response body.
type apiError struct {
	Kind   apiKind
	Status int
	Reason string // short and fixed in form: safe to store and log
}

func (e *apiError) Error() string { return e.Reason }

type chatResult struct {
	Content      string
	FinishReason string
	TokensIn     int
	TokensOut    int
	Cost         float64
}

type openRouter struct {
	base    string
	key     string
	http    *http.Client
	backoff time.Duration // first wait between tries (doubles each time)
}

type errorBody struct {
	Error struct {
		Code     any    `json:"code"`
		Message  string `json:"message"`
		Metadata struct {
			LimitSource  string `json:"limit_source"`
			Reasons      []any  `json:"reasons"`
			FlaggedInput any    `json:"flagged_input"`
		} `json:"metadata"`
	} `json:"error"`
}

const (
	limitInFlight = "openrouter_in_flight_budget"
)

func creditLimitSource(s string) bool {
	switch s {
	case "openrouter_key_limit", "openrouter_workspace_budget", "openrouter_guardrail_budget":
		return true
	}
	return false
}

// classify turns a failed response into an error and, for a temporary one, how long to wait
// (0: use the backoff). It looks at error.metadata.limit_source before the status alone.
func classify(status int, hdr http.Header, body []byte) (kind apiKind, retryAfter time.Duration, reason string) {
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	md := eb.Error.Metadata
	if ra, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); err == nil && ra > 0 {
		retryAfter = min(time.Duration(ra)*time.Second, maxRetryAfter)
	}
	code := strconv.Itoa(status)
	switch {
	case status == http.StatusUnauthorized:
		return apiAuth, 0, "HTTP 401: invalid key"
	case md.LimitSource == limitInFlight:
		if retryAfter > 0 {
			return apiTemporary, retryAfter, "HTTP " + code + ": too much in flight"
		}
		return apiStopCredit, 0, "HTTP " + code + ": in-flight budget"
	case creditLimitSource(md.LimitSource):
		return apiStopCredit, 0, "HTTP " + code + ": " + md.LimitSource
	case status == http.StatusPaymentRequired:
		return apiStopCredit, 0, "HTTP 402: out of credit"
	case status == http.StatusForbidden:
		if len(md.Reasons) > 0 || md.FlaggedInput != nil {
			return apiBlocked, 0, "HTTP 403: moderation block"
		}
		return apiBlocked, 0, "HTTP 403: not permitted"
	case status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500:
		return apiTemporary, retryAfter, "HTTP " + code
	}
	return apiOther, 0, "HTTP " + code
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do sends one request and retries temporary failures. It returns the body of a success.
func (c *openRouter) do(ctx context.Context, method, path string, payload []byte) ([]byte, *apiError) {
	wait := c.backoff
	if wait <= 0 {
		wait = defaultBackoff
	}
	var last *apiError
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, &apiError{Kind: apiOther, Reason: "cancelled"}
			}
			wait *= 2
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
		if err != nil {
			return nil, &apiError{Kind: apiOther, Reason: "bad request"}
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Title", "openipdata rdns codes")
		resp, err := c.http.Do(req)
		if err != nil {
			// The error text holds the URL, never the key, but it is not needed: say what kind.
			if ctx.Err() != nil {
				return nil, &apiError{Kind: apiOther, Reason: "cancelled"}
			}
			last = &apiError{Kind: apiTemporary, Reason: "network error or timeout"}
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if rerr != nil {
			last = &apiError{Kind: apiTemporary, Status: resp.StatusCode, Reason: "response read failed"}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			// OpenRouter can report a failure inside a 200 body.
			var eb errorBody
			if json.Unmarshal(data, &eb) == nil && eb.Error.Message != "" {
				status := resp.StatusCode
				switch v := eb.Error.Code.(type) {
				case float64:
					status = int(v)
				case string:
					if n, e := strconv.Atoi(v); e == nil {
						status = n
					}
				}
				kind, ra, reason := classify(status, resp.Header, data)
				if kind == apiTemporary {
					last = &apiError{Kind: kind, Status: status, Reason: reason}
					if ra > 0 {
						wait = ra
					}
					continue
				}
				return nil, &apiError{Kind: kind, Status: status, Reason: reason}
			}
			return data, nil
		}
		kind, ra, reason := classify(resp.StatusCode, resp.Header, data)
		last = &apiError{Kind: kind, Status: resp.StatusCode, Reason: reason}
		if kind != apiTemporary {
			return nil, last
		}
		if ra > 0 {
			wait = ra
		}
	}
	return nil, last
}

// chat asks one question: JSON answer, temperature 0, reasoning effort low.
func (c *openRouter) chat(ctx context.Context, model, prompt string) (chatResult, *apiError) {
	payload, _ := json.Marshal(map[string]any{
		"model":           model,
		"temperature":     0,
		"max_tokens":      maxOutputTokens,
		"messages":        []map[string]string{{"role": "user", "content": prompt}},
		"response_format": map[string]string{"type": "json_object"},
		"usage":           map[string]bool{"include": true},
		"reasoning":       map[string]string{"effort": "low"},
	})
	data, aerr := c.do(ctx, http.MethodPost, "/chat/completions", payload)
	if aerr != nil {
		return chatResult{}, aerr
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &r) != nil || len(r.Choices) == 0 {
		return chatResult{}, &apiError{Kind: apiOther, Reason: "unreadable response"}
	}
	res := chatResult{Content: r.Choices[0].Message.Content, FinishReason: r.Choices[0].FinishReason,
		TokensIn: r.Usage.PromptTokens, TokensOut: r.Usage.CompletionTokens, Cost: r.Usage.Cost}
	if strings.TrimSpace(res.Content) == "" {
		return res, &apiError{Kind: apiOther, Reason: "empty answer (finish " + res.FinishReason + ")"}
	}
	return res, nil
}

// keyRemaining reads GET /key: the credit the key has left (nil: no limit).
func (c *openRouter) keyRemaining(ctx context.Context) (*float64, *apiError) {
	data, aerr := c.do(ctx, http.MethodGet, "/key", nil)
	if aerr != nil {
		return nil, aerr
	}
	var r struct {
		Data struct {
			LimitRemaining *float64 `json:"limit_remaining"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &r) != nil {
		return nil, &apiError{Kind: apiOther, Reason: "unreadable key info"}
	}
	return r.Data.LimitRemaining, nil
}
