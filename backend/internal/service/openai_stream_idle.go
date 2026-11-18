package service

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *OpenAIGatewayService) openAIStreamIdleFailover(ctx context.Context, c *gin.Context, account *Account, passthrough bool, requestID, model string, headers http.Header) *UpstreamFailoverError {
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTimeout(ctx, account, model)
	}
	err := s.newOpenAIStreamFailoverErrorWithModel(c, account, passthrough, requestID, nil,
		"OpenAI upstream stream idle timeout before model output", model, headers)
	err.SafeToFailoverAfterWrite = true
	err.RetryableOnSameAccount = false
	return err
}

func (s *OpenAIGatewayService) openAISynchronousStreamIdleReader(resp *http.Response, account *Account) (io.Reader, func() bool) {
	if account == nil || account.Platform != PlatformOpenAI || s.cfg == nil || s.cfg.Gateway.StreamDataIntervalTimeout <= 0 {
		return resp.Body, func() bool { return false }
	}
	reader := newOpenAIStreamIdleReader(resp.Body, time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout)*time.Second)
	return reader, reader.stop
}

// openAIStreamIdleReader lets synchronous SSE readers interrupt a stalled
// upstream read without moving downstream writes onto a different goroutine.
type openAIStreamIdleReader struct {
	body     io.ReadCloser
	interval time.Duration
	timer    *time.Timer
	mu       sync.Mutex
	lastRead time.Time
	expired  bool
	stopped  bool
}

func newOpenAIStreamIdleReader(body io.ReadCloser, interval time.Duration) *openAIStreamIdleReader {
	r := &openAIStreamIdleReader{body: body, interval: interval}
	r.mu.Lock()
	r.lastRead = time.Now()
	r.timer = time.AfterFunc(interval, r.checkDeadline)
	r.mu.Unlock()
	return r
}

func (r *openAIStreamIdleReader) checkDeadline() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	if remaining := time.Until(r.lastRead.Add(r.interval)); remaining > 0 {
		r.timer.Reset(remaining)
		r.mu.Unlock()
		return
	}
	r.expired = true
	r.mu.Unlock()
	_ = r.body.Close()
}

func (r *openAIStreamIdleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.mu.Lock()
		r.lastRead = time.Now()
		r.mu.Unlock()
	}
	return n, err
}

func (r *openAIStreamIdleReader) stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.timer.Stop()
	return r.expired
}
