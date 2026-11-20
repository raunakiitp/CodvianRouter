package cache

import (
	"context"
	"os"
	"testing"
)

// TestB2RedisLiveSetGetSyncReopen pins the Redis semantic tier
// (:738) against a REAL Redis. It runs only when
// CODVIAN_TEST_REDIS_ADDR is set (e.g. 127.0.0.1:6379) — the VM
// has no Redis, so the proof runs cold on the PC against the
// WSL Redis 6.0.16 on 127.0.0.1:6379 (localhost forwarding).
// Product minimum is Redis >= 4.0: writeRow uses variadic HSet,
// which the old native Windows Redis 3.2.100 does NOT support
// (HSET f1 v1 f2 v2 → ERR on 3.2; HMSET works) — do not point
// this test at the 3.2 server.
// Without the env var the test skips; with it set and Redis down,
// OpenRedisSemanticCache errors and the test FAILS — the tier is
// pinned to a live server, not to a mock. Flow: open → SetForModel
// (vector API, write-through) → GetForModel hit → Sync → Close →
// reopen with the same prefix → GetForModel hit again (load()
// reloads the row from Redis) → cleanup.
func TestB2RedisLiveSetGetSyncReopen(t *testing.T) {
	addr := os.Getenv("CODVIAN_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("CODVIAN_TEST_REDIS_ADDR unset — live Redis proof runs on the PC only")
	}
	ctx := context.Background()
	cfg := LocalSemanticCacheConfig{
		NeuralEmbedder: (&countingEmbedder{}).embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	}
	prefix := "codvian-test-b2-738:"
	rc, err := OpenRedisSemanticCache(RedisOptions{Addr: addr}, prefix, cfg)
	if err != nil {
		t.Fatalf("OpenRedisSemanticCache(%q): %v", addr, err)
	}
	if err := rc.SetForModel(ctx, "m", neuralVecAlpha(), neuralTestEntry("redis-hit")); err != nil {
		t.Fatalf("SetForModel: %v", err)
	}
	got, hit := rc.GetForModel(ctx, "m", neuralVecAlpha())
	if !hit || got.Response.Content != "redis-hit" {
		t.Fatalf("GetForModel after Set: hit=%v content=%q, want hit with redis-hit", hit, got.Response.Content)
	}
	n, err := rc.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n < 1 {
		t.Fatalf("Sync: wrote %d entries, want >= 1", n)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rc2, err := OpenRedisSemanticCache(RedisOptions{Addr: addr}, prefix, cfg)
	if err != nil {
		t.Fatalf("reopen OpenRedisSemanticCache(%q): %v", addr, err)
	}
	defer func() {
		_ = rc2.deleteAll(ctx)
		_ = rc2.Close()
	}()
	got, hit = rc2.GetForModel(ctx, "m", neuralVecAlpha())
	if !hit || got.Response.Content != "redis-hit" {
		t.Fatalf("GetForModel after reopen: hit=%v content=%q, want hit with redis-hit", hit, got.Response.Content)
	}
}
