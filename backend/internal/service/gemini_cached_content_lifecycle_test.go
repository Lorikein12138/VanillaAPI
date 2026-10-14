//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type blockingGeminiCachedContentRepo struct {
	*geminiCachedContentRepoStub
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	enteredDo sync.Once
}

func (r *blockingGeminiCachedContentRepo) ListStaleMutations(ctx context.Context, _ int) ([]int64, error) {
	r.enteredDo.Do(func() { close(r.entered) })
	<-ctx.Done()
	close(r.cancelled)
	<-r.release
	return nil, ctx.Err()
}

type countingGeminiCachedContentRepo struct {
	*geminiCachedContentRepoStub
	calls   atomic.Int32
	entered chan struct{}
}

type failingGeminiCachedContentRepo struct {
	*geminiCachedContentRepoStub
	entered chan struct{}
	called  sync.Once
}

type r2GeminiCachedContentRecoveryRepo struct {
	*geminiCachedContentRepoStub
	staleIDs           []int64
	recordsByID        map[int64]*GeminiCachedContent
	listOnceOnly       bool
	listCalls          atomic.Int32
	takeOverCalls      atomic.Int32
	releaseCalls       atomic.Int32
	softDeleteCalls    atomic.Int32
	listEntered        chan struct{}
	allowList          chan struct{}
	takeOverEntered    chan struct{}
	allowTakeOver      chan struct{}
	takeOverReturnsNil bool
	listOnce           sync.Once
	takeOverOnce       sync.Once
	mu                 sync.Mutex
	takeOverIDs        []int64
}

func newR2GeminiCachedContentRecoveryRepo(ids ...int64) *r2GeminiCachedContentRecoveryRepo {
	repo := &r2GeminiCachedContentRecoveryRepo{
		geminiCachedContentRepoStub: newGeminiCachedContentRepoStub(),
		staleIDs:                    append([]int64(nil), ids...),
		recordsByID:                 make(map[int64]*GeminiCachedContent, len(ids)),
	}
	for _, id := range ids {
		record := &GeminiCachedContent{ID: id, PublicID: fmt.Sprintf("cached-%d", id), AccountID: 91, UpstreamName: fmt.Sprintf("cachedContents/%d", id), ExpireTime: time.Now().Add(time.Hour)}
		repo.recordsByID[id] = record
		repo.records[record.PublicID] = record
	}
	return repo
}

func (r *r2GeminiCachedContentRecoveryRepo) ListStaleMutations(ctx context.Context, _ int) ([]int64, error) {
	call := r.listCalls.Add(1)
	if r.listEntered != nil {
		r.listOnce.Do(func() { close(r.listEntered) })
	}
	if r.allowList != nil {
		select {
		case <-r.allowList:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.listOnceOnly && call > 1 {
		return nil, nil
	}
	return append([]int64(nil), r.staleIDs...), nil
}

func (r *r2GeminiCachedContentRecoveryRepo) TakeOverStaleMutation(_ context.Context, id int64, _ string, _ time.Duration) (*GeminiCachedContent, error) {
	r.takeOverCalls.Add(1)
	r.mu.Lock()
	r.takeOverIDs = append(r.takeOverIDs, id)
	r.mu.Unlock()
	if r.takeOverEntered != nil {
		r.takeOverOnce.Do(func() { close(r.takeOverEntered) })
	}
	if r.allowTakeOver != nil {
		<-r.allowTakeOver
	}
	if r.takeOverReturnsNil {
		return nil, nil
	}
	return r.recordsByID[id], nil
}

func (r *r2GeminiCachedContentRecoveryRepo) ReleaseMutation(context.Context, int64, string) error {
	r.releaseCalls.Add(1)
	return nil
}

func (r *r2GeminiCachedContentRecoveryRepo) SoftDelete(ctx context.Context, id int64) error {
	r.softDeleteCalls.Add(1)
	return r.geminiCachedContentRepoStub.SoftDelete(ctx, id)
}

type r2GeminiCachedContentAccountRepo struct {
	AccountRepository
	account *Account
}

func (r r2GeminiCachedContentAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

type r2GeminiCachedContentUpstream struct {
	calls     atomic.Int32
	entered   chan struct{}
	cancelled chan struct{}
}

func (u *r2GeminiCachedContentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	close(u.entered)
	<-req.Context().Done()
	close(u.cancelled)
	return nil, req.Context().Err()
}

func (u *r2GeminiCachedContentUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

type r2GeminiTokenCacheStub struct{ GeminiTokenCache }

func (r2GeminiTokenCacheStub) GetAccessToken(context.Context, string) (string, error) {
	return "cached-token", nil
}
func (r2GeminiTokenCacheStub) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}
func (r2GeminiTokenCacheStub) DeleteAccessToken(context.Context, string) error { return nil }
func (r2GeminiTokenCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}
func (r2GeminiTokenCacheStub) ReleaseRefreshLock(context.Context, string) error { return nil }

func (r *failingGeminiCachedContentRepo) ListStaleMutations(context.Context, int) ([]int64, error) {
	r.called.Do(func() { close(r.entered) })
	return nil, errors.New("database unavailable")
}

func (r *countingGeminiCachedContentRepo) ListStaleMutations(context.Context, int) ([]int64, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	return nil, nil
}

func TestGeminiCachedContentStopWaitsForRecovery(t *testing.T) {
	sink, restoreLog := captureStructuredLog(t)
	defer restoreLog()

	repo := &blockingGeminiCachedContentRepo{
		geminiCachedContentRepoStub: newGeminiCachedContentRepoStub(),
		entered:                     make(chan struct{}),
		cancelled:                   make(chan struct{}),
		release:                     make(chan struct{}),
	}
	svc := NewGeminiCachedContentService(repo, nil, nil)
	svc.recoveryInterval = time.Millisecond
	svc.Start()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not enter ListStaleMutations")
	}

	stopReturned := make(chan struct{})
	go func() {
		svc.Stop()
		close(stopReturned)
	}()
	select {
	case <-repo.cancelled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the in-flight repository call")
	}
	select {
	case <-stopReturned:
		t.Fatal("Stop returned before the in-flight recovery exited")
	case <-time.After(50 * time.Millisecond):
		// Keep the repository call blocked through an observable wait window;
		// the release channel below is the only way recovery can finish.
	}
	close(repo.release)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after recovery exited")
	}
	for _, event := range lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed") {
		require.NotEqual(t, "warn", event.Level, "normal cancellation must not emit a recovery warning")
	}

	// Repeated Stop remains safe after the worker has joined.
	svc.Stop()
}

func TestGeminiCachedContentStopBeforeStart(t *testing.T) {
	repo := &countingGeminiCachedContentRepo{
		geminiCachedContentRepoStub: newGeminiCachedContentRepoStub(),
		entered:                     make(chan struct{}),
	}
	svc := NewGeminiCachedContentService(repo, nil, nil)
	svc.recoveryInterval = time.Millisecond
	svc.Stop()
	svc.Start()

	select {
	case <-repo.entered:
		t.Fatal("Start after Stop-before-Start must not launch recovery")
	default:
	}
	require.True(t, svc.stopping)
	require.False(t, svc.started)
	require.Zero(t, repo.calls.Load())
	svc.Stop()
}

func TestGeminiCachedContentConcurrentStartStop(t *testing.T) {
	sink, restoreLog := captureStructuredLog(t)
	defer restoreLog()

	repo := &countingGeminiCachedContentRepo{
		geminiCachedContentRepoStub: newGeminiCachedContentRepoStub(),
		entered:                     make(chan struct{}),
	}
	svc := NewGeminiCachedContentService(repo, nil, nil)
	svc.recoveryInterval = time.Millisecond

	var starters sync.WaitGroup
	for range 16 {
		starters.Add(1)
		go func() {
			defer starters.Done()
			svc.Start()
		}()
	}
	starters.Wait()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("concurrent Start calls did not launch recovery")
	}

	var stoppers sync.WaitGroup
	for range 16 {
		stoppers.Add(1)
		go func() {
			defer stoppers.Done()
			svc.Stop()
		}()
	}
	stoppers.Wait()
	stoppedAt := repo.calls.Load()
	svc.Start()
	require.Equal(t, stoppedAt, repo.calls.Load(), "no recovery may run after Stop completes")

	failingRepo := &failingGeminiCachedContentRepo{
		geminiCachedContentRepoStub: newGeminiCachedContentRepoStub(),
		entered:                     make(chan struct{}),
	}
	failingSvc := NewGeminiCachedContentService(failingRepo, nil, nil)
	failingSvc.recoveryInterval = time.Millisecond
	failingSvc.Start()
	select {
	case <-failingRepo.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not call the failing repository")
	}
	require.Eventually(t, func() bool {
		return len(lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed")) > 0
	}, time.Second, time.Millisecond, "a real repository failure must remain visible as a warning")
	for _, event := range lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed") {
		require.Equal(t, "warn", event.Level)
	}
	failingSvc.Stop()
}

func TestGeminiCachedContentExplicitRecoveryAfterStop(t *testing.T) {
	repo := newR2GeminiCachedContentRecoveryRepo(1)
	svc := NewGeminiCachedContentService(repo, nil, nil)
	svc.Stop()

	recovered, err := svc.RecoverStaleMutations(context.Background())

	require.NoError(t, err)
	require.Zero(t, recovered)
	require.Zero(t, repo.listCalls.Load(), "explicit recovery after Stop must not list stale mutations")
	require.Zero(t, repo.takeOverCalls.Load())
	require.Zero(t, repo.releaseCalls.Load())
	require.Zero(t, repo.softDeleteCalls.Load())
	require.Len(t, repo.records, 1, "the pending mutation remains persisted")
}

func TestGeminiCachedContentExplicitRecoveryStopsBetweenItems(t *testing.T) {
	t.Run("stop at list return boundary", func(t *testing.T) {
		sink, restoreLog := captureStructuredLog(t)
		defer restoreLog()
		repo := newR2GeminiCachedContentRecoveryRepo(1, 2)
		repo.listEntered = make(chan struct{})
		repo.allowList = make(chan struct{})
		svc := NewGeminiCachedContentService(repo, nil, nil)
		type result struct {
			recovered int
			err       error
		}
		finished := make(chan result, 1)
		go func() {
			recovered, err := svc.RecoverStaleMutations(context.Background())
			finished <- result{recovered: recovered, err: err}
		}()
		select {
		case <-repo.listEntered:
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not enter the stale-list call")
		}

		svc.Stop()
		close(repo.allowList)
		select {
		case got := <-finished:
			require.NoError(t, got.err)
			require.Zero(t, got.recovered)
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not return after the stale-list boundary")
		}
		require.EqualValues(t, 1, repo.listCalls.Load())
		require.Zero(t, repo.takeOverCalls.Load(), "Stop at the list boundary must prevent every item takeover")
		require.Zero(t, repo.releaseCalls.Load())
		require.Zero(t, repo.softDeleteCalls.Load())
		require.Len(t, repo.records, 2, "both mutation intents remain persisted")
		require.Empty(t, lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed"))
		require.Empty(t, lifecycleEventsContaining(sink, "Warning: gemini cached content"))
	})

	t.Run("stop while first item lookup is in flight", func(t *testing.T) {
		sink, restoreLog := captureStructuredLog(t)
		defer restoreLog()
		repo := newR2GeminiCachedContentRecoveryRepo(1, 2)
		repo.takeOverEntered = make(chan struct{})
		repo.allowTakeOver = make(chan struct{})
		repo.takeOverReturnsNil = true
		svc := NewGeminiCachedContentService(repo, nil, nil)
		type result struct {
			recovered int
			err       error
		}
		finished := make(chan result, 1)
		go func() {
			recovered, err := svc.RecoverStaleMutations(context.Background())
			finished <- result{recovered: recovered, err: err}
		}()
		select {
		case <-repo.takeOverEntered:
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not enter the first item lookup")
		}

		svc.Stop()
		close(repo.allowTakeOver)
		select {
		case got := <-finished:
			require.NoError(t, got.err)
			require.Zero(t, got.recovered)
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not stop before the second item")
		}
		require.EqualValues(t, 1, repo.takeOverCalls.Load())
		require.Equal(t, []int64{1}, repo.takeOverIDs)
		require.Zero(t, repo.releaseCalls.Load(), "Stop must leave the first pending intent untouched")
		require.Zero(t, repo.softDeleteCalls.Load())
		require.Len(t, repo.records, 2, "neither mutation intent is released or deleted")
		require.Empty(t, lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed"))
		require.Empty(t, lifecycleEventsContaining(sink, "Warning: gemini cached content"))
	})
}

func TestGeminiCachedContentStopCancelsRecoveryUpstream(t *testing.T) {
	sink, restoreLog := captureStructuredLog(t)
	defer restoreLog()
	repo := newR2GeminiCachedContentRecoveryRepo(1)
	repo.listOnceOnly = true
	upstream := &r2GeminiCachedContentUpstream{entered: make(chan struct{}), cancelled: make(chan struct{})}
	accountRepo := r2GeminiCachedContentAccountRepo{account: &Account{
		ID: 91, Platform: PlatformGemini, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "test-api-key", "base_url": "https://generativelanguage.googleapis.com/v1beta",
			GeminiExplicitCacheUpstreamCredentialKey: true,
		},
	}}
	tokenProvider := NewGeminiTokenProvider(nil, r2GeminiTokenCacheStub{}, nil)
	cfg := &config.Config{}
	compat := NewGeminiMessagesCompatService(accountRepo, nil, nil, nil, tokenProvider, nil, upstream, nil, cfg)
	svc := NewGeminiCachedContentService(repo, compat, cfg)
	svc.recoveryInterval = time.Millisecond
	svc.Start()
	select {
	case <-upstream.entered:
	case <-time.After(2 * time.Second):
		svc.Stop()
		t.Fatal("stale mutation recovery did not reach the upstream call")
	}

	svc.Stop()
	select {
	case <-upstream.cancelled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the recovery request context")
	}
	require.EqualValues(t, 1, upstream.calls.Load())
	require.EqualValues(t, 1, repo.takeOverCalls.Load())
	require.Zero(t, repo.releaseCalls.Load(), "cancelled recovery must preserve the unfinished mutation intent")
	require.Zero(t, repo.softDeleteCalls.Load())
	require.Len(t, repo.records, 1)
	require.Empty(t, lifecycleEventsContaining(sink, "gemini cached content mutation recovery failed"))
	require.Empty(t, lifecycleEventsContaining(sink, "Warning: gemini cached content"))
}

// commitBlockingGeminiCachedContentRepo 在 SoftDelete 落库之后阻塞，用来让 Stop 恰好落在
// 「单条恢复已提交、外层尚未记账」之间。
type commitBlockingGeminiCachedContentRepo struct {
	*r2GeminiCachedContentRecoveryRepo
	commitErr   error
	committed   chan struct{}
	allowReturn chan struct{}
	commitOnce  sync.Once
}

func (r *commitBlockingGeminiCachedContentRepo) SoftDelete(ctx context.Context, id int64) error {
	err := r.r2GeminiCachedContentRecoveryRepo.SoftDelete(ctx, id)
	r.commitOnce.Do(func() { close(r.committed) })
	<-r.allowReturn
	if r.commitErr != nil {
		return r.commitErr
	}
	return err
}

func TestGeminiCachedContentExplicitRecoveryKeepsCommittedResultWhenStopped(t *testing.T) {
	run := func(t *testing.T, commitErr error) (int, error, *commitBlockingGeminiCachedContentRepo) {
		t.Helper()
		repo := &commitBlockingGeminiCachedContentRepo{
			r2GeminiCachedContentRecoveryRepo: newR2GeminiCachedContentRecoveryRepo(1, 2),
			commitErr:                         commitErr,
			committed:                         make(chan struct{}),
			allowReturn:                       make(chan struct{}),
		}
		// 绑定账号已不存在：单条恢复不触达上游，直接以 SoftDelete 提交。
		compat := NewGeminiMessagesCompatService(r2GeminiCachedContentAccountRepo{}, nil, nil, nil, nil, nil, nil, nil, &config.Config{})
		svc := NewGeminiCachedContentService(repo, compat, &config.Config{})
		type result struct {
			recovered int
			err       error
		}
		finished := make(chan result, 1)
		go func() {
			recovered, err := svc.RecoverStaleMutations(context.Background())
			finished <- result{recovered: recovered, err: err}
		}()
		select {
		case <-repo.committed:
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not commit the first item")
		}
		svc.Stop()
		close(repo.allowReturn)
		select {
		case got := <-finished:
			require.Equal(t, []int64{1}, repo.takeOverIDs, "Stop after the first commit must prevent the second takeover")
			return got.recovered, got.err, repo
		case <-time.After(time.Second):
			t.Fatal("explicit recovery did not return after Stop")
		}
		return 0, nil, repo
	}

	t.Run("committed recovery is counted", func(t *testing.T) {
		recovered, err, repo := run(t, nil)
		require.NoError(t, err)
		require.Equal(t, 1, recovered, "a recovery committed before Stop must be counted")
		require.EqualValues(t, 1, repo.softDeleteCalls.Load())
		require.Len(t, repo.records, 1, "the second mutation intent remains persisted")
	})

	t.Run("real commit error is kept", func(t *testing.T) {
		commitErr := errors.New("soft delete failed")
		recovered, err, _ := run(t, commitErr)
		require.Zero(t, recovered)
		require.ErrorIs(t, err, commitErr, "a real error returned before Stop must not be dropped")
		require.ErrorContains(t, err, "cached content 1")
	})
}
