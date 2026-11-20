#!/usr/bin/env python3
"""Load generator for the GC-profile benchmark (bench-gc-profile.sh).

Stdlib only: N concurrent workers POST distinct prompts to the router's
/v1/chat/completions and record per-request latency. Prompts are
lexically distinct (a different topic phrase per prompt) so the router's
semantic cache (token-overlap, threshold 0.85) and single-flight
coalescing do not collapse the load into cache hits — every request
must travel to the mock upstream.

Usage:

    python3 bench-loadgen.py --url http://127.0.0.1:20128 \
        --key <gateway-key> --requests 2000 --concurrency 20

Prints one JSON object to stdout: requests, errors, mean_ms, p50_ms,
p95_ms, p99_ms, throughput_rps, elapsed_s.
"""
import argparse
import json
import statistics
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

TOPICS = [
    "photosynthesis in desert cacti", "tidal locking of exoplanets",
    "sourdough starter hydration ratios", "Byzantine fault tolerance proofs",
    "migrating monarch butterflies", "welding aluminium bicycle frames",
    "Old Norse saga manuscripts", "fermentation of kimchi at altitude",
    "orbital rendezvous mathematics", "handloom weaving in Kanchipuram",
    "quantum error correction codes", "composting kitchen scraps safely",
    "medieval siege engine design", "deep-sea hydrothermal vent ecology",
    "restoring vintage fountain pens", "solar sail navigation principles",
    "beekeeping in rooftop apiaries", "cartography of uncharted islands",
    "ferrofluid sculpture techniques", "Andean potato domestication history",
    "luthier wood selection secrets", "glacier retreat measurements",
    "Morse code transmission speed", "bonsai pruning seasonal schedules",
    "ant colony optimisation algorithms", "Victorian greenhouse heating",
    "kayak rolling Eskimo technique", "fungal mycelium network signalling",
    "brass instrument mouthpiece acoustics", "Saharan caravan trade routes",
    "origami tessellation folding patterns", "coral bleaching recovery studies",
    "blacksmith forge temperature control", "Arctic permafrost thaw feedback",
    "calligraphy ink grinding methods", "seismic wave propagation models",
    "urban pigeon navigation abilities", "tapestry weaving loom setups",
    "volcanic ash soil fertility", "lighthouse lens Fresnel design",
    "mushroom foraging safety rules", "river delta sediment deposition",
    "typewriter ribbon replacement steps", "penguin huddling thermodynamics",
    "stained glass leading techniques", "monsoon rainfall prediction models",
    "harpsichord tuning temperaments", "cave painting pigment chemistry",
    "saffron harvesting labour costs", "wind turbine blade erosion",
]


def one(url, key, prompt):
    body = json.dumps({
        "model": "model-a",
        "messages": [{"role": "user", "content": "Explain %s in one sentence." % prompt}],
    }).encode()
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Authorization": "Bearer " + key,
        "Content-Type": "application/json",
    })
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            resp.read()
            if resp.status != 200:
                return None
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError):
        return None
    return (time.perf_counter() - t0) * 1000.0


def percentile(sorted_vals, pct):
    if not sorted_vals:
        return None
    i = min(len(sorted_vals) - 1, max(0, int(pct / 100.0 * len(sorted_vals))))
    return sorted_vals[i]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--requests", type=int, default=2000)
    ap.add_argument("--concurrency", type=int, default=20)
    args = ap.parse_args()

    endpoint = args.url.rstrip("/") + "/v1/chat/completions"
    lat = []
    errors = 0
    t0 = time.perf_counter()
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futs = [pool.submit(one, endpoint, args.key, TOPICS[i % len(TOPICS)])
                for i in range(args.requests)]
        for f in futs:
            ms = f.result()
            if ms is None:
                errors += 1
            else:
                lat.append(ms)
    elapsed = time.perf_counter() - t0
    lat.sort()
    out = {
        "requests": args.requests,
        "ok": len(lat),
        "errors": errors,
        "elapsed_s": round(elapsed, 3),
        "throughput_rps": round(len(lat) / elapsed, 1) if elapsed else None,
        "mean_ms": round(statistics.mean(lat), 3) if lat else None,
        "p50_ms": round(percentile(lat, 50), 3) if lat else None,
        "p95_ms": round(percentile(lat, 95), 3) if lat else None,
        "p99_ms": round(percentile(lat, 99), 3) if lat else None,
    }
    print(json.dumps(out))
    # Exit 0 even with errors: errors are data for the harness to
    # report, not a reason to abort the whole benchmark run.
    return 0


if __name__ == "__main__":
    sys.exit(main())
