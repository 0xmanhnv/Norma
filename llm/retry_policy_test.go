package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const okStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

func streamAll(p Provider) (text string, err error) {
	acc := NewAccumulator()
	for ev, e := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}}) {
		if e != nil {
			return acc.Message().Text(), e
		}
		acc.Add(ev)
	}
	return acc.Message().Text(), nil
}

// The wait before a retry: the server's Retry-After when it asks for longer
// than our backoff, otherwise the exponential step drawn from its upper half,
// never more than the cap.
func TestRetryWait(t *testing.T) {
	lo := func() float64 { return 0 }
	hi := func() float64 { return 0.999999 }
	hdr := func(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }
	var c Config

	// Jitter: attempt 2 has a 2s step, so the wait is in [1s, 2s).
	if got := retryWait(c, 2, nil, lo); got != time.Second {
		t.Fatalf("low end of the jitter range: %v, want 1s", got)
	}
	if got := retryWait(c, 2, nil, hi); got < 1990*time.Millisecond || got >= 2*time.Second {
		t.Fatalf("high end of the jitter range: %v, want just under 2s", got)
	}
	// Retry-After in seconds, in milliseconds and as a date.
	if got := retryWait(c, 0, hdr("Retry-After", "7"), hi); got != 7*time.Second {
		t.Fatalf("Retry-After: 7 → %v", got)
	}
	if got := retryWait(c, 0, hdr("Retry-After-Ms", "1500"), hi); got != 1500*time.Millisecond {
		t.Fatalf("retry-after-ms: 1500 → %v", got)
	}
	date := time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryWait(c, 0, hdr("Retry-After", date), hi); got < 18*time.Second || got > 21*time.Second {
		t.Fatalf("Retry-After date 20s ahead → %v", got)
	}
	// A shorter Retry-After does not shorten our own backoff.
	if got := retryWait(c, 4, hdr("Retry-After", "1"), lo); got != 4*time.Second {
		t.Fatalf("Retry-After shorter than the backoff: %v, want 4s", got)
	}
	// The cap bounds both the ladder and what a server may ask for.
	c.RetryMaxDelay = 3 * time.Second
	if got := retryWait(c, 10, nil, hi); got > 3*time.Second {
		t.Fatalf("backoff above the cap: %v", got)
	}
	if got := retryWait(c, 0, hdr("Retry-After", "3600"), lo); got != 3*time.Second {
		t.Fatalf("Retry-After above the cap: %v, want 3s", got)
	}
	// A fixed interval is not jittered, but still yields to Retry-After.
	c = Config{RetryInterval: 250 * time.Millisecond}
	if got := retryWait(c, 3, nil, hi); got != 250*time.Millisecond {
		t.Fatalf("fixed interval: %v", got)
	}
	if got := retryWait(c, 3, hdr("Retry-After", "2"), hi); got != 2*time.Second {
		t.Fatalf("fixed interval with Retry-After: %v", got)
	}
	// Garbage is ignored.
	if _, ok := retryAfter(hdr("Retry-After", "soon"), time.Now()); ok {
		t.Fatal("unparseable Retry-After accepted")
	}
}

// End to end: a 429 that names its wait is retried after that wait, not after
// our (much shorter) interval.
func TestRetryAfterIsHonoured(t *testing.T) {
	var calls atomic.Int32
	var second time.Time
	first := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first = time.Now()
			w.Header().Set("retry-after-ms", "150")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		second = time.Now()
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", RetryInterval: time.Millisecond})
	if text, err := streamAll(p); err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
	if gap := second.Sub(first); gap < 140*time.Millisecond {
		t.Fatalf("retried after %v; the server asked for 150ms", gap)
	}
}

// 529 (overloaded) and the other transient statuses are retried; a 400 is not.
func TestRetryableStatuses(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504, 529} {
		if !isRetryableStatus(code) {
			t.Fatalf("%d must be retried", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 413, 422} {
		if isRetryableStatus(code) {
			t.Fatalf("%d must not be retried", code)
		}
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", RetryInterval: time.Millisecond})
	if text, err := streamAll(p); err != nil || text != "ok" || calls.Load() != 3 {
		t.Fatalf("text=%q err=%v calls=%d; want ok after two 529s", text, err, calls.Load())
	}
	// Exhausted, a 529 is reported as the provider being unavailable.
	calls.Store(-100)
	p, _ = NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: 1, RetryInterval: time.Millisecond})
	if _, err := streamAll(p); !IsCode(err, CodeUnavailable) {
		t.Fatalf("exhausted 529: code %q, want %q (%v)", CodeOf(err), CodeUnavailable, err)
	}
}

// hijackClose drops the connection without writing a response.
func hijackClose(t *testing.T, w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	conn.Close()
}

// A connection that is reset before the response starts is retried: nothing
// was delivered, so sending the request again cannot duplicate anything.
func TestConnectionResetBeforeFirstByteIsRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hijackClose(t, w)
			return
		}
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", RetryInterval: time.Millisecond})
	if text, err := streamAll(p); err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2 (reset, then success)", calls.Load())
	}
}

// Once a stream has produced output it is never replayed: a drop mid-stream is
// an error to the caller, who has already seen (and possibly acted on) the
// first part. Sending the request again would produce that part twice.
func TestStreamIsNotReplayedAfterFirstByte(t *testing.T) {
	for _, format := range []Format{FormatOpenAI, FormatAnthropic} {
		t.Run(string(format), func(t *testing.T) {
			first := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
			if format == FormatAnthropic {
				first = "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				// Promise more than is sent, then drop the connection: the
				// client sees the first event and then a broken body.
				w.Header().Set("content-length", "100000")
				w.Write([]byte(first))
				w.(http.Flusher).Flush()
				hijackClose(t, w)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: format, BaseURL: srv.URL, APIKey: "k", Model: "m", RetryInterval: time.Millisecond})
			text, err := streamAll(p)
			if err == nil {
				t.Fatal("a stream cut mid-way must surface an error")
			}
			if text != "partial" {
				t.Fatalf("delivered %q before the error, want the first event once", text)
			}
			if calls.Load() != 1 {
				t.Fatalf("the request was sent %d times; a started stream must never be replayed", calls.Load())
			}
		})
	}
}

// A stream that goes silent fails after StreamIdleTimeout instead of hanging,
// with the provider-unavailable code, and is not sent again.
func TestStreamIdleTimeout(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select { // then nothing: no bytes, no close
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m",
		RetryInterval: time.Millisecond, StreamIdleTimeout: 100 * time.Millisecond})
	start := time.Now()
	text, err := streamAll(p)
	if err == nil || !IsCode(err, CodeUnavailable) || !strings.Contains(err.Error(), "stream idle") {
		t.Fatalf("err=%v (code %q), want a stream-idle CodeUnavailable error", err, CodeOf(err))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller's context did not time out: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v to notice a silent stream", took)
	}
	if text != "partial" || calls.Load() != 1 {
		t.Fatalf("text=%q calls=%d; want the first event once and no replay", text, calls.Load())
	}
}

// A stream that keeps sending is not cut by the idle timeout however long it
// runs in total: the timer measures silence, not duration.
func TestStreamIdleTimeoutResetsOnData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 6 {
			w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m", StreamIdleTimeout: 250 * time.Millisecond})
	if text, err := streamAll(p); err != nil || text != "xxxxxx" {
		t.Fatalf("text=%q err=%v; a live stream longer than the idle timeout must complete", text, err)
	}
}
