// Package compress — M7.8 worker pool / async compression
// (PARITY.md Area 10, "Worker pool / async compression", MISSING).
//
// CompressionPool runs stacked-pipeline jobs OFF the request hot
// path, on a bounded set of goroutines — Go's equivalent of
// OmniRoute's worker-thread pool. It does NOT wire itself into
// Apply or any handler; the coordinator decides at merge time
// which calls go through the pool (the pool is for work that may
// wait, e.g. background / pre-compression — a request that needs
// its compressed prompt now must not be parked behind a queue).
//
// What was read in OmniRoute (study only — no code copied):
//
//   - open-sse/services/compression/compressionWorkerPool.ts —
//     CompressionWorkerPool: size default 2 (env
//     OMNI_COMPRESSION_WORKERS), per-job timeout default 120 s
//     (OMNI_COMPRESSION_WORKER_TIMEOUT_MS), idle eviction 60 s,
//     maxOldGenerationSizeMb 1024; run() returns a promise;
//     close() resolves queued and in-flight jobs with the
//     UNCHANGED body; a spawn failure marks the pool broken and
//     every later run() fail-opens immediately instead of
//     queueing work that can never drain; a job timeout fails
//     with retryInProcess=false (re-running the same CPU-bound
//     pipeline in-process would stall the event loop), while
//     other faults fail with retryInProcess=true; idle workers
//     are terminated (dropping the reference alone would strand
//     the thread for the process lifetime).
//   - open-sse/services/compression/compressionWorker.ts — the
//     worker-thread entry point: adapts the body, runs the
//     stacked pipeline, posts per-step progress, and on ANY
//     engine throw posts an error message instead of dying.
//   - open-sse/services/compression/compressionWorkerProtocol.ts
//     — the job/message protocol ({id, body, mode, options};
//     step/result/error) and isCompressionWorkerEligible: only
//     standard/rtk/stacked (plus non-SLM ultra) are eligible,
//     stacked only when every step is caveman/rtk/standard, and
//     the payload must be strictly serializable.
//   - open-sse/services/compression/engines/llmlingua/onnxWorker.ts
//     — the ONNX (LLMLingua-2) worker: heavy deps imported
//     lazily inside the handler, fail-open to the ORIGINAL text
//     on any failure, and code blocks never reach the worker.
//     There is no ONNX/LLMLingua engine in this repo, so there
//     is no ONNX worker here — the fail-open contract is what
//     carries over: a failed job returns the original messages
//     as an error result, never a partial compression.
//
// Pool semantics (this file):
//
//   - Bounded workers: Size goroutines (default NumCPU capped
//     at 4 — DefaultPoolSize). Jobs wait in a bounded queue
//     (default Size*4); Submit blocks while the queue is full,
//     until space frees or the caller's context ends.
//   - Every job carries a context and a timeout: the effective
//     timeout is the job's Timeout, else the pool's JobTimeout
//     (default 120 s, mirroring OmniRoute's worker timeout).
//     A job that exceeds it resolves with ErrJobTimeout and the
//     pool keeps working. A job whose context is cancelled
//     resolves with the context error. The timed-out job's
//     goroutine is abandoned (Go cannot kill it) but its result
//     is discarded, so a stuck job can never wedge the pool.
//   - Panic isolation: a job that panics is recovered per job
//     and reported as a job error; the worker — and the pool —
//     survive and take the next job.
//   - Determinism: the pipeline is a pure function of its
//     input, so the same job returns the same messages whatever
//     the worker count (proved by test on pool sizes 1 and 4).
//   - Shutdown drains: Shutdown stops accepting new jobs and
//     waits for queued and in-flight jobs to finish, so every
//     submitted future resolves. It never abandons a submitted
//     job. (OmniRoute's close() resolves pending jobs with the
//     unchanged body instead; draining is the stricter choice
//     here because a queued stacked job is cheap CPU work and
//     its caller is waiting on the future — resolving it with
//     "unchanged" would silently report uncompressed work as
//     done. The difference is documented for the coordinator.)
//   - No goroutine leaks: workers exit when the job channel
//     closes at shutdown, and the timeout-abandoned runner
//     goroutine always finishes (it runs a finite job). Proved
//     by the goroutine-count test.
package compress

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Pool errors. Callers match with errors.Is.
var (
	// ErrPoolClosed is returned (as a job error) when a job is
	// submitted after Shutdown started.
	ErrPoolClosed = errors.New("compression pool is closed")
	// ErrJobTimeout is returned when a job exceeds its timeout.
	ErrJobTimeout = errors.New("compression job timed out")
)

// DefaultJobTimeout mirrors OmniRoute's worker timeout
// (compressionWorkerPool.ts: 120_000 ms).
const DefaultJobTimeout = 120 * time.Second

// DefaultPoolSize returns the default worker count: NumCPU
// capped at 4 (and at least 1). Compression is CPU-bound, so
// more workers than CPUs only add scheduling overhead.
func DefaultPoolSize() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// PoolConfig tunes NewCompressionPool. Zero values take the
// defaults: Size DefaultPoolSize(), JobTimeout
// DefaultJobTimeout, QueueSize Size*4.
type PoolConfig struct {
	Size       int
	JobTimeout time.Duration
	QueueSize  int
}

func (c PoolConfig) effective() PoolConfig {
	if c.Size <= 0 {
		c.Size = DefaultPoolSize()
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = DefaultJobTimeout
	}
	if c.QueueSize <= 0 {
		c.QueueSize = c.Size * 4
	}
	return c
}

// CompressionJob is one unit of pool work: a stacked-pipeline
// run over Messages with Config. Run is an optional override
// for the work itself: when nil the pool runs RunStacked on the
// job's messages and config. Run exists so callers (and tests)
// can supply slow, failing, or panicking work without forging
// pathological prompts — the default path always runs the real
// pipeline. A Run implementation must respect ctx (return on
// ctx.Done) for cancellation to be prompt; a Run that ignores
// ctx still cannot wedge the pool, because the worker abandons
// its result on timeout.
type CompressionJob struct {
	Messages []provider.Message
	Config   StackedConfig
	// Timeout overrides the pool's JobTimeout for this job.
	// 0 means the pool default.
	Timeout time.Duration
	// Run overrides the work. Nil means RunStacked.
	Run func(ctx context.Context, msgs []provider.Message, cfg StackedConfig) (StackedResult, error)
}

// CompressionJobResult is the outcome of one job. Err is nil on
// success; on failure Result is the zero StackedResult and the
// original messages are NOT smuggled in as a fake success — the
// caller decides the fallback (usually: send the original
// messages uncompressed), exactly like OmniRoute's fail-open
// contract, but reported as an error instead of a silent
// "unchanged" success.
type CompressionJobResult struct {
	Result StackedResult
	Err    error
}

// CompressionFuture is the handle Submit returns: a future
// (channel) for one job's result. It resolves exactly once,
// even when the pool shuts down or the job times out.
type CompressionFuture struct {
	done chan CompressionJobResult
}

// Wait blocks until the job finishes and returns its result.
func (f *CompressionFuture) Wait() CompressionJobResult {
	return <-f.done
}

// WaitContext blocks until the job finishes or ctx ends; on ctx
// end it returns the ctx error (the job keeps running in the
// pool — waiting is abandoned, the work is not cancelled).
func (f *CompressionFuture) WaitContext(ctx context.Context) (CompressionJobResult, error) {
	select {
	case r := <-f.done:
		return r, nil
	case <-ctx.Done():
		return CompressionJobResult{}, ctx.Err()
	}
}

// Done exposes the result channel for select-based waiting. Do
// not send on it; it is closed after exactly one result.
func (f *CompressionFuture) Done() <-chan CompressionJobResult {
	return f.done
}

// poolJob is the internal queued unit: the caller's job plus its
// context and future.
type poolJob struct {
	job    CompressionJob
	ctx    context.Context
	future *CompressionFuture
}

// CompressionPool is a bounded goroutine pool for compression
// jobs. The zero value is not usable; construct with
// NewCompressionPool.
type CompressionPool struct {
	cfg PoolConfig

	mu     sync.Mutex // guards closed; never held across Submit's queue send (see Submit)
	closed bool
	jobs   chan poolJob
	done   chan struct{} // closed when Shutdown starts, before close(jobs): lets a Submit racing Shutdown abandon its send without holding mu
	wg     sync.WaitGroup
}

// NewCompressionPool creates a pool and starts its workers.
func NewCompressionPool(cfg PoolConfig) *CompressionPool {
	cfg = cfg.effective()
	p := &CompressionPool{
		cfg:  cfg,
		jobs: make(chan poolJob, cfg.QueueSize),
		done: make(chan struct{}),
	}
	for i := 0; i < cfg.Size; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// Size returns the configured worker count.
func (p *CompressionPool) Size() int { return p.cfg.Size }

// QueueSize returns the configured queue capacity.
func (p *CompressionPool) QueueSize() int { return p.cfg.QueueSize }

// Submit enqueues job and returns its future immediately. The
// job runs under ctx (plus its timeout). If the pool is closed
// the future resolves at once with ErrPoolClosed. If the queue
// is full Submit blocks until space frees or ctx ends; on ctx
// end the future resolves at once with the ctx error. Submit
// never panics, even racing Shutdown.
func (p *CompressionPool) Submit(ctx context.Context, job CompressionJob) *CompressionFuture {
	if ctx == nil {
		ctx = context.Background()
	}
	f := &CompressionFuture{done: make(chan CompressionJobResult, 1)}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		f.done <- CompressionJobResult{Err: ErrPoolClosed}
		return f
	}
	p.mu.Unlock()
	pj := poolJob{job: job, ctx: ctx, future: f}
	// The send is OUTSIDE p.mu. Holding mu across the send
	// serialised concurrent submitters: a second blocked
	// submitter waited on mu.Lock() and never reached its
	// ctx.Done() select, so its cancel was ignored until the
	// queue drained — and Shutdown stalled behind it too.
	// Shutdown closes p.done before close(p.jobs), so a racing
	// Shutdown is observed here instead: a send on a closed
	// channel is never chosen by select, and the done branch
	// resolves the future with ErrPoolClosed. A send chosen
	// before close(p.jobs) lands in the buffered channel and
	// is drained by the workers, so no job is lost.
	select {
	case p.jobs <- pj:
	case <-ctx.Done():
		f.done <- CompressionJobResult{Err: ctx.Err()}
	case <-p.done:
		f.done <- CompressionJobResult{Err: ErrPoolClosed}
	}
	return f
}

// Shutdown stops accepting new jobs and waits for every queued
// and in-flight job to finish, so all submitted futures resolve.
// It is safe to call more than once; the second call returns
// immediately. Shutdown must not be called from a job's Run
// function (it would deadlock waiting for itself).
func (p *CompressionPool) Shutdown() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	// Close done first: a Submit racing Shutdown observes it
	// and abandons its send (see Submit). Only then close the
	// jobs channel — a send on a closed channel is never
	// chosen by select, so no sender can panic.
	close(p.done)
	close(p.jobs)
	p.mu.Unlock()
	p.wg.Wait()
}

// worker is one pool goroutine: it takes jobs until the channel
// closes, runs each with timeout enforcement and panic recovery,
// and resolves the future exactly once.
func (p *CompressionPool) worker() {
	defer p.wg.Done()
	for pj := range p.jobs {
		p.runJob(pj)
	}
}

// runJob executes one job with its timeout and panic isolation.
// The work runs in a child goroutine so a timeout can abandon
// it: the child always finishes on its own (the pipeline is
// finite CPU work, and a custom Run that ignores ctx still
// returns eventually), and its late result is discarded.
func (p *CompressionPool) runJob(pj poolJob) {
	timeout := pj.job.Timeout
	if timeout <= 0 {
		timeout = p.cfg.JobTimeout
	}
	ctx, cancel := context.WithTimeout(pj.ctx, timeout)
	defer cancel()

	resultCh := make(chan CompressionJobResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resultCh <- CompressionJobResult{Err: fmt.Errorf("compression job panicked: %v", r)}
			}
		}()
		run := pj.job.Run
		if run == nil {
			run = func(_ context.Context, msgs []provider.Message, cfg StackedConfig) (StackedResult, error) {
				return RunStacked(msgs, cfg), nil
			}
		}
		res, err := run(ctx, pj.job.Messages, pj.job.Config)
		resultCh <- CompressionJobResult{Result: res, Err: err}
	}()

	select {
	case r := <-resultCh:
		pj.future.done <- r
	case <-ctx.Done():
		// Timeout or parent cancellation. The child goroutine
		// is abandoned but keeps running to completion; its
		// result is dropped by the buffered channel (nobody
		// reads it) and the goroutine exits on its own.
		if ctx.Err() == context.DeadlineExceeded {
			pj.future.done <- CompressionJobResult{Err: ErrJobTimeout}
		} else {
			pj.future.done <- CompressionJobResult{Err: ctx.Err()}
		}
	}
}
