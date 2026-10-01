package service

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func newContentModerationTestService(t *testing.T, settings SettingRepository, repo ContentModerationRepository, hashes ContentModerationHashCache, groups GroupRepository, users UserRepository, proxies ProxyRepository, invalidator APIKeyAuthCacheInvalidator, email *EmailService) *ContentModerationService {
	t.Helper()
	svc := NewContentModerationService(settings, repo, hashes, groups, users, proxies, invalidator, email)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, svc.Shutdown(ctx))
	})
	return svc
}

type lifecycleBlockedLogRepo struct {
	contentModerationTestRepo
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *lifecycleBlockedLogRepo) CreateLog(ctx context.Context, log *ContentModerationLog) error {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return r.contentModerationTestRepo.CreateLog(ctx, log)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestContentModerationShutdownLifecycle(t *testing.T) {
	t.Run("nil_zero_idempotent_concurrent", func(t *testing.T) {
		var nilSvc *ContentModerationService
		require.NoError(t, nilSvc.Shutdown(context.Background()))
		svc := &ContentModerationService{}
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() { defer wg.Done(); require.NoError(t, svc.Shutdown(context.Background())) }()
		}
		wg.Wait()
		require.NoError(t, svc.Shutdown(context.Background()))
	})
	t.Run("drain_and_reject", func(t *testing.T) {
		repo := &contentModerationTestRepo{}
		svc := newContentModerationTestService(t, &contentModerationRuntimeSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
		cfg := defaultContentModerationConfig()
		for range 50 {
			svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{Action: ContentModerationActionAllow}, "", false, false)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, svc.Shutdown(ctx))
		require.Len(t, repo.snapshotLogs(), 50)
		svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{}, "", false, false)
		svc.enqueueAsync(ContentModerationCheckInput{}, cfg, ContentModerationInput{}, "")
		require.Equal(t, int64(50), svc.asyncEnqueued.Load())
		require.Equal(t, int64(2), svc.asyncDropped.Load())
	})
	t.Run("no_consumer_deadline", func(t *testing.T) {
		svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = svc.Shutdown(ctx)
			select {
			case <-svc.shutdownDone:
			case <-time.After(30 * time.Second):
				t.Error("cleanup did not join coordinator")
			}
		})
		svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, svc.Shutdown(ctx), context.DeadlineExceeded)
		select {
		case <-svc.shutdownDone:
		case <-time.After(30 * time.Second):
			t.Fatal("shutdown coordinator leaked")
		}
		require.ErrorIs(t, svc.Shutdown(context.Background()), context.DeadlineExceeded)
	})
	t.Run("stop_admission_while_draining", func(t *testing.T) {
		repo := &lifecycleBlockedLogRepo{started: make(chan struct{}), release: make(chan struct{})}
		svc := newContentModerationTestService(t, &contentModerationRuntimeSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
		var once sync.Once
		unblock := func() { once.Do(func() { close(repo.release) }) }
		t.Cleanup(unblock)
		svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
		select {
		case <-repo.started:
		case <-time.After(30 * time.Second):
			t.Fatal("task did not start")
		}
		maintenance := svc.runtimeContext(true)
		finished := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			finished <- svc.Shutdown(ctx)
		}()
		select {
		case <-maintenance.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("shutdown did not stop admission")
		}
		svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
		require.Equal(t, int64(1), svc.asyncDropped.Load())
		select {
		case <-finished:
			t.Fatal("shutdown completed before accepted task")
		default:
		}
		unblock()
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("drain did not finish")
		}
		require.Len(t, repo.snapshotLogs(), 1)
	})
	t.Run("cancel_running_io", func(t *testing.T) {
		repo := &lifecycleBlockedLogRepo{started: make(chan struct{}), release: make(chan struct{})}
		svc := NewContentModerationService(&contentModerationRuntimeSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
		t.Cleanup(func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = svc.Shutdown(ctx)
			select {
			case <-svc.shutdownDone:
			case <-time.After(30 * time.Second):
				t.Error("cleanup did not join workers")
			}
		})
		svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
		select {
		case <-repo.started:
		case <-time.After(30 * time.Second):
			t.Fatal("task did not start")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, svc.Shutdown(ctx), context.Canceled)
		select {
		case <-svc.shutdownDone:
		case <-time.After(30 * time.Second):
			t.Fatal("IO cancellation did not join workers")
		}
	})
	t.Run("refresh_add_and_shutdown", func(t *testing.T) {
		svc := newContentModerationTestService(t, &contentModerationRuntimeSettingRepo{}, &contentModerationTestRepo{}, nil, nil, nil, nil, nil, nil)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 20 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; svc.triggerRuntimeSnapshotRefresh() }()
		}
		close(start)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, svc.Shutdown(ctx))
		wg.Wait()
	})
	t.Run("refresh_registration_and_cancel", func(t *testing.T) {
		repo := &contentModerationRuntimeSettingRepo{}
		svc := &ContentModerationService{settingRepo: repo}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, svc.Shutdown(ctx))
		})
		_, err := svc.loadRuntimeSnapshot(context.Background())
		require.NoError(t, err)
		started, release := make(chan struct{}, 1), make(chan struct{})
		repo.blockNextMultiple(started, release)
		svc.triggerRuntimeSnapshotRefresh()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("refresh did not start")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, svc.Shutdown(ctx), "shutdown cancels maintenance IO")
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() { defer wg.Done(); svc.triggerRuntimeSnapshotRefresh() }()
		}
		wg.Wait()
		_, calls := repo.calls()
		require.Equal(t, 2, calls, "refresh cannot register after shutdown")
	})
}

func TestContentModerationFixturesReleaseResources(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	initial := runtime.NumGoroutine()
	for i := range 30 {
		t.Run("fixture", func(t *testing.T) {
			svc := newContentModerationTestService(t, &contentModerationRuntimeSettingRepo{}, &contentModerationTestRepo{}, nil, nil, nil, nil, nil, nil)
			require.Equal(t, 100000, cap(svc.asyncQueue))
			require.Equal(t, 32, svc.workerCount)
			require.GreaterOrEqual(t, runtime.NumGoroutine(), initial+32)
			t.Logf("iteration=%d task_bytes=%d queue_cap=%d workers=%d goroutines=%d", i, unsafe.Sizeof(contentModerationTask{}), cap(svc.asyncQueue), svc.workerCount, runtime.NumGoroutine())
		})
	}
	// Collection observes queues after their actual workers have joined.
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("goroutines before=%d after=%d heap before=%d after=%d", initial, runtime.NumGoroutine(), before.HeapAlloc, after.HeapAlloc)
	require.LessOrEqual(t, runtime.NumGoroutine(), initial+5)
	require.Less(t, after.HeapAlloc, before.HeapAlloc+64*1024*1024)
}
