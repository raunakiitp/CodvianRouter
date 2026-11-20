package compress

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Toggle-evidence convention: pool behaviour is asserted both
// through the pool and against the direct (no-pool) run. A
// test that passes with the pool bypassed — e.g. determinism
// checked against itself — fails the cross-check; a test that
// passes with a broken pool fails the pool assertion.

func poolFixture() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "tool", Content: "=== RUN TestAlpha\n--- PASS: TestAlpha (0.01s)\n=== RUN TestBeta\n--- FAIL: TestBeta (0.01s)\n    beta_test.go:7: boom\nFAIL\tgithub.com/example/pkg\t0.03s"},
		{Role: "user", Content: "Hello, could you please explain in detail how the caching system works in this project? I really appreciate it, thank you so much for your help."},
	}
}

func stackedPoolJob(msgs []provider.Message) CompressionJob {
	return CompressionJob{Messages: msgs, Config: StackedConfig{Availability: allEngines()}}
}

func TestPoolDefaultSize(t *testing.T) {
	size := DefaultPoolSize()
	if size < 1 || size > 4 {
		t.Errorf("DefaultPoolSize() = %d, want 1..4", size)
	}
	if want := runtime.NumCPU(); want <= 4 && size != want {
		t.Errorf("DefaultPoolSize() = %d, want NumCPU %d", size, want)
	}
	if want := 4; runtime.NumCPU() > 4 && size != want {
		t.Errorf("DefaultPoolSize() = %d, want cap %d", size, want)
	}
	p := NewCompressionPool(PoolConfig{})
	defer p.Shutdown()
	if p.Size() != size {
		t.Errorf("pool Size() = %d, want %d", p.Size(), size)
	}
	if p.QueueSize() != size*4 {
		t.Errorf("pool QueueSize() = %d, want %d", p.QueueSize(), size*4)
	}
}

func TestPoolRunsStackedJob(t *testing.T) {
	msgs := poolFixture()
	p := NewCompressionPool(PoolConfig{Size: 2})
	defer p.Shutdown()
	f := p.Submit(context.Background(), stackedPoolJob(msgs))
	r := f.Wait()
	if r.Err != nil {
		t.Fatalf("pool job error: %v", r.Err)
	}
	if !r.Result.Applied {
		t.Fatalf("pool job applied nothing")
	}
	// Toggle: the pool result must equal the direct (no-pool)
	// RunStacked result — the pool adds concurrency, not
	// different compression.
	direct := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	if !reflect.DeepEqual(r.Result.Messages, direct.Messages) {
		t.Errorf("pool result differs from direct RunStacked")
	}
	if r.Result.AfterTokens != direct.AfterTokens {
		t.Errorf("pool AfterTokens %d, want direct %d", r.Result.AfterTokens, direct.AfterTokens)
	}
}

func TestPoolTimeout(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 1})
	defer p.Shutdown()
	blocking := CompressionJob{
		Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
			<-ctx.Done()
			return StackedResult{}, ctx.Err()
		},
	}
	r := p.Submit(context.Background(), blocking).Wait()
	if !errors.Is(r.Err, ErrJobTimeout) {
		t.Fatalf("timeout job error = %v, want ErrJobTimeout", r.Err)
	}
	// Toggle: the pool survives a timeout — the next job runs.
	ok := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
	if ok.Err != nil || !ok.Result.Applied {
		t.Fatalf("pool unusable after timeout: err=%v applied=%v", ok.Err, ok.Result.Applied)
	}
}

func TestPoolCancel(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 1})
	defer p.Shutdown()
	// Submit with an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := p.Submit(ctx, stackedPoolJob(poolFixture())).Wait()
	if !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("cancelled submit error = %v, want context.Canceled", r.Err)
	}
	// Cancel an in-flight job: its Run waits on ctx.Done.
	ctx2, cancel2 := context.WithCancel(context.Background())
	blocking := CompressionJob{
		Run: func(ctx context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
			<-ctx.Done()
			return StackedResult{}, ctx.Err()
		},
	}
	f := p.Submit(ctx2, blocking)
	time.Sleep(50 * time.Millisecond)
	cancel2()
	r = f.Wait()
	if !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("cancelled in-flight job error = %v, want context.Canceled", r.Err)
	}
	// Pool still works after a cancellation.
	ok := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
	if ok.Err != nil {
		t.Fatalf("pool unusable after cancel: %v", ok.Err)
	}
}

func TestPoolPanicRecovery(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 1})
	defer p.Shutdown()
	panicking := CompressionJob{
		Run: func(_ context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
			panic("boom in test job")
		},
	}
	r := p.Submit(context.Background(), panicking).Wait()
	if r.Err == nil || !strings.Contains(r.Err.Error(), "panicked") {
		t.Fatalf("panicking job error = %v, want a panic error", r.Err)
	}
	// Toggle: the worker — and the pool — survive the panic.
	ok := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
	if ok.Err != nil || !ok.Result.Applied {
		t.Fatalf("pool unusable after panic: err=%v applied=%v", ok.Err, ok.Result.Applied)
	}
}

func TestPoolDeterminism(t *testing.T) {
	msgs := poolFixture()
	job := stackedPoolJob(msgs)
	p1 := NewCompressionPool(PoolConfig{Size: 1})
	r1 := p1.Submit(context.Background(), job).Wait()
	p1.Shutdown()
	p4 := NewCompressionPool(PoolConfig{Size: 4})
	r4 := p4.Submit(context.Background(), job).Wait()
	p4.Shutdown()
	if r1.Err != nil || r4.Err != nil {
		t.Fatalf("determinism jobs errored: %v / %v", r1.Err, r4.Err)
	}
	if !reflect.DeepEqual(r1.Result.Messages, r4.Result.Messages) {
		t.Errorf("same job on pool sizes 1 and 4 gave different messages")
	}
	if r1.Result.AfterTokens != r4.Result.AfterTokens {
		t.Errorf("same job on pool sizes 1 and 4 gave different AfterTokens: %d vs %d", r1.Result.AfterTokens, r4.Result.AfterTokens)
	}
	// Toggle: both also equal the direct run.
	direct := RunStacked(msgs, StackedConfig{Availability: allEngines()})
	if !reflect.DeepEqual(r1.Result.Messages, direct.Messages) {
		t.Errorf("pool size 1 result differs from direct RunStacked")
	}
}

func TestPoolShutdownDrains(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 2, QueueSize: 8})
	var futures []*CompressionFuture
	for i := 0; i < 5; i++ {
		futures = append(futures, p.Submit(context.Background(), stackedPoolJob(poolFixture())))
	}
	p.Shutdown()
	// Every submitted future resolves with a real result —
	// shutdown drains, it never abandons a submitted job.
	for i, f := range futures {
		select {
		case r := <-f.Done():
			if r.Err != nil || !r.Result.Applied {
				t.Errorf("job %d after shutdown: err=%v applied=%v", i, r.Err, r.Result.Applied)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("job %d future never resolved after shutdown", i)
		}
	}
	// Submit after shutdown resolves at once with ErrPoolClosed.
	r := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
	if !errors.Is(r.Err, ErrPoolClosed) {
		t.Fatalf("post-shutdown submit error = %v, want ErrPoolClosed", r.Err)
	}
	// Shutdown is idempotent.
	p.Shutdown()
}

func TestPoolNoGoroutineLeak(t *testing.T) {
	settle := func() {
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
	}
	settle()
	base := runtime.NumGoroutine()
	p := NewCompressionPool(PoolConfig{Size: 2})
	for i := 0; i < 4; i++ {
		r := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
		if r.Err != nil {
			t.Fatalf("job %d error: %v", i, r.Err)
		}
	}
	// A timed-out job's abandoned runner must also exit: its
	// Run returns on ctx.Done (the pool's timeout), so the
	// worker's select resolves and the runner goroutine ends.
	slow := CompressionJob{
		Timeout: 20 * time.Millisecond,
		Run: func(ctx context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
			select {
			case <-ctx.Done():
				return StackedResult{}, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return StackedResult{}, nil
			}
		},
	}
	_ = p.Submit(context.Background(), slow).Wait()
	p.Shutdown()
	// Poll for the goroutine count to return to baseline: the
	// pool's workers must exit at shutdown, and the abandoned
	// runner must finish on its own.
	deadline := time.Now().Add(5 * time.Second)
	for {
		settle()
		if runtime.NumGoroutine() <= base+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines, baseline %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPoolFutureWaitContext(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 1})
	defer p.Shutdown()
	blocking := CompressionJob{
		Timeout: 300 * time.Millisecond,
		Run: func(ctx context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
			<-ctx.Done()
			return StackedResult{}, ctx.Err()
		},
	}
	f := p.Submit(context.Background(), blocking)
	// Waiting with a short context gives up waiting (but does
	// not cancel the job — the job keeps running in the pool).
	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.WaitContext(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitContext error = %v, want context.DeadlineExceeded", err)
	}
	// The job itself resolves on its own timeout shortly after.
	r := f.Wait()
	if !errors.Is(r.Err, ErrJobTimeout) {
		t.Fatalf("abandoned job error = %v, want ErrJobTimeout", r.Err)
	}
	// The pool is still healthy afterwards.
	ok := p.Submit(context.Background(), stackedPoolJob(poolFixture())).Wait()
	if ok.Err != nil {
		t.Fatalf("pool unusable after WaitContext abandon: %v", ok.Err)
	}
}

// TestPoolSecondBlockedSubmitterCancel: Submit's documented
// contract is "blocks until space frees or the caller's ctx
// ends". Before the fix, Submit held the pool mutex across the
// blocking queue send, so a second concurrently-blocked
// submitter waited on mu.Lock() and never reached its
// ctx.Done() select — its cancel was ignored until the queue
// drained (and Shutdown stalled behind it too). Re-review
// defect (ProbeJ).
func TestPoolSecondBlockedSubmitterCancel(t *testing.T) {
	p := NewCompressionPool(PoolConfig{Size: 1, QueueSize: 1})
	hold := make(chan struct{})
	blocking := CompressionJob{Run: func(ctx context.Context, _ []provider.Message, _ StackedConfig) (StackedResult, error) {
		<-hold
		return StackedResult{}, nil
	}}
	// Occupies the worker, then fills the queue.
	f0 := p.Submit(context.Background(), blocking)
	_ = p.Submit(context.Background(), stackedPoolJob(poolFixture()))
	// First blocked submitter.
	firstDone := make(chan *CompressionFuture, 1)
	go func() { firstDone <- p.Submit(context.Background(), stackedPoolJob(poolFixture())) }()
	time.Sleep(200 * time.Millisecond)
	// Second blocked submitter with a cancellable ctx.
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan *CompressionFuture, 1)
	go func() { secondDone <- p.Submit(ctx, stackedPoolJob(poolFixture())) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case f2 := <-secondDone:
		r := f2.Wait()
		if !errors.Is(r.Err, context.Canceled) {
			t.Fatalf("second blocked submitter: err = %v, want context.Canceled", r.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second blocked submitter did not resolve within 2s of cancel (ctx cancel ignored)")
	}
	// Cleanup: release the worker, let the queued job and the
	// first blocked submitter drain, then shut down.
	close(hold)
	_ = f0.Wait()
	select {
	case f1 := <-firstDone:
		_ = f1.Wait()
	case <-time.After(2 * time.Second):
		t.Fatal("first blocked submitter did not enqueue after the queue drained")
	}
	p.Shutdown()
}
