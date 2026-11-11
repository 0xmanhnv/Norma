package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// doStream sends the request with retries on transient failures and returns a
// 200 response (caller reads + closes the body). Only the request
// establishment phase is retried — a connection error or reset, a timeout, or
// a retryable status, all before any response data is consumed — so retrying
// is safe: it can't duplicate partially-streamed assistant output. A
// mid-stream drop is the caller's to surface, never to replay. Context
// cancellation is never retried and interrupts the backoff.
//
// Count comes from cfg.retries(); the wait before each retry from retryWait
// (the server's Retry-After when it sends one, jittered exponential backoff
// otherwise, a fixed wait when Config.RetryInterval is set). With
// Config.StreamIdleTimeout the returned body fails once it has been silent for
// that long.
func doStream(ctx context.Context, cfg Config, url string, body []byte, setHeaders func(*http.Request), errPrefix string) (*http.Response, error) {
	// Rate limit once per logical request (outside the retry loop, so retries do
	// not consume additional tokens). Shared across all callers of this provider.
	if err := cfg.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	retries := cfg.retries()
	client := cfg.HTTPClient
	if client == nil { // NewProvider always sets one; a direct caller may not
		client = http.DefaultClient
	}
	for attempt := 0; ; attempt++ {
		// Each attempt gets its own cancel, so an idle body can abort its
		// request without touching the caller's context.
		rctx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		setHeaders(req)

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body = newWatchedBody(resp.Body, cancel, cfg.StreamIdleTimeout, errPrefix)
			return resp, nil
		}

		retryable := false
		switch {
		case err != nil:
			retryable = ctx.Err() == nil // transient network error (not cancellation)
		case isRetryableStatus(resp.StatusCode):
			retryable = true
		}
		if attempt >= retries || !retryable {
			defer cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil, err // cancellation is the caller's, not the provider's
				}
				return nil, WrapError(CodeUnavailable, "", err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(data))
			return nil, NewError(classifyStatus(resp.StatusCode, body),
				fmt.Sprintf("%s: status %d: %s", errPrefix, resp.StatusCode, body))
		}
		var header http.Header
		if resp != nil {
			header = resp.Header
			resp.Body.Close()
		}
		cancel()
		if !backoffSleep(ctx, retryWait(cfg, attempt, header, rand.Float64)) {
			return nil, ctx.Err() // context cancelled during backoff
		}
	}
}

// isRetryableStatus reports whether a status is worth sending the request
// again for: the provider was busy or briefly broken, not the request wrong.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		statusOverloaded:               // 529
		return true
	}
	return false
}

// statusOverloaded is the non-standard 529 some providers (Anthropic among
// them) answer with when they are overloaded.
const statusOverloaded = 529

// expBackoff is the default retry ladder: 0.5s, 1s, 2s, … doubling per attempt.
// Config.retryDelay caps it. Used whenever the caller did not pin a fixed
// interval on the Config.
func expBackoff(attempt int) time.Duration {
	if attempt > 20 { // far past any cap; avoid shifting into overflow
		attempt = 20
	}
	return 500 * time.Millisecond * (1 << attempt)
}

// retryWait is how long to wait before retry number attempt (0-based) of a
// request the server answered with header (nil for a network error).
//
//   - A server that says when to come back (Retry-After) is believed: retrying
//     earlier is a wasted request that can extend the limit. Its wait is used
//     when it is longer than ours.
//   - Otherwise the exponential step, drawn at random from its upper half
//     ("equal jitter"): many clients that hit the same limit at the same time
//     must not all return at the same instant. rnd returns a value in [0,1).
//   - A fixed Config.RetryInterval is used as is, with no jitter.
//
// Every wait is capped at Config.RetryMaxDelay.
func retryWait(cfg Config, attempt int, header http.Header, rnd func() float64) time.Duration {
	d := cfg.retryDelay(attempt)
	if cfg.RetryInterval <= 0 {
		d = d/2 + time.Duration(rnd()*float64(d/2))
	}
	if ra, ok := retryAfter(header, time.Now()); ok && ra > d {
		d = ra
	}
	return min(d, cfg.maxRetryDelay())
}

// retryAfter reads how long a response asks the client to wait: the
// millisecond form some providers send ("retry-after-ms"), else the standard
// Retry-After as a number of seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// watchedBody is a response body that releases its request's context when
// closed and, with an idle timeout, fails once no byte has arrived for that
// long. A stalled stream otherwise blocks in Read until the caller's context
// ends, which for a long-running agent can be never.
type watchedBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer
	fired  atomic.Bool
	prefix string
}

func newWatchedBody(rc io.ReadCloser, cancel context.CancelFunc, idle time.Duration, prefix string) *watchedBody {
	b := &watchedBody{ReadCloser: rc, cancel: cancel, idle: idle, prefix: prefix}
	if idle > 0 {
		b.timer = time.AfterFunc(idle, func() {
			b.fired.Store(true)
			cancel() // unblocks the Read in progress
		})
	}
	return b
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.timer != nil && n > 0 && !b.fired.Load() {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF && b.fired.Load() {
		return n, WrapError(CodeUnavailable, fmt.Sprintf("%s: stream idle for %v", b.prefix, b.idle), err)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// backoffSleep waits d, interruptible by ctx. Returns false if the context was
// cancelled (or d is non-positive, which never happens for a real wait).
func backoffSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
