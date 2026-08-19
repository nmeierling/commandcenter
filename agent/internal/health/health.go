// Package health runs HTTP probes against project health endpoints.
package health

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/devcc/agent/internal/model"
)

// Checker performs health probes with a shared, bounded HTTP client.
type Checker struct {
	client *http.Client
}

// New returns a checker with the given per-request timeout.
func New(timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Checker{
		client: &http.Client{
			Timeout: timeout,
			// Health endpoints commonly redirect (/ -> /login). Following the
			// redirect would report the wrong thing, so we treat any response
			// as proof the server is up and let ExpectStatus decide.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
				MaxIdleConnsPerHost: 2,
				DisableKeepAlives:   true,
			},
		},
	}
}

// Check probes the configured URL.
func (c *Checker) Check(ctx context.Context, hc *model.HealthCheck) model.HealthResult {
	res := model.HealthResult{Checked: true, At: time.Now()}
	if hc == nil || hc.URL == "" {
		return model.HealthResult{}
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hc.URL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "devcc-agent/1.0 (health check)")

	resp, err := c.client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = friendly(err)
		return res
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	res.StatusCode = resp.StatusCode
	if hc.ExpectStatus > 0 {
		res.OK = resp.StatusCode == hc.ExpectStatus
	} else {
		res.OK = resp.StatusCode >= 200 && resp.StatusCode < 500
	}
	if !res.OK {
		res.Error = http.StatusText(resp.StatusCode)
	}
	return res
}

// friendly trims Go's verbose transport errors down to something a dashboard
// can display in one line.
func friendly(err error) string {
	var netErr net.Error
	if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
		return "timed out"
	}
	var opErr *net.OpError
	if asOpError(err, &opErr) {
		if opErr.Op == "dial" {
			return "connection refused"
		}
	}
	return err.Error()
}

func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*target = ne
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func asOpError(err error, target **net.OpError) bool {
	for err != nil {
		if oe, ok := err.(*net.OpError); ok {
			*target = oe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
