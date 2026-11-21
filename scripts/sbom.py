#!/usr/bin/env python3
"""sbom.py — CycloneDX-1.4 SBOM from `go list -m -json all`.

Stdlib-only producer (G-06). Reproducible for a given go.mod/go.sum: the
component graph is deterministic; only the serialNumber and timestamp
vary. Run from the repo root.

Usage:
  python3 scripts/sbom.py [--out PATH] [--self-check]
"""
import json
import os
import subprocess
import sys
import time

SER = int(time.time() * 1000) % (10**16)  # pseudo-uuid suffix for the serial


def go_modules(root):
    out = subprocess.run(["go", "list", "-m", "-json", "all"], cwd=root,
                         capture_output=True, text=True)
    if out.returncode != 0:
        raise RuntimeError("go list -m -json all failed: " + out.stderr[-500:])
    # `go list -m -json all` emits one JSON object per module back to
    # back with no separator; parse the stream with a JSONDecoder.
    dec = json.JSONDecoder()
    mods = []
    data = out.stdout
    i = 0
    while i < len(data):
        while i < len(data) and data[i] in " \t\r\n":
            i += 1
        if i >= len(data):
            break
        try:
            obj, end = dec.raw_decode(data, i)
        except json.JSONDecodeError:
            break
        mods.append(obj)
        i = end
    seen = set()
    result = []
    for m in mods:
        path = m.get("Path", "")
        if not path or path in seen:
            continue
        seen.add(path)
        ver = m.get("Version") or "devel"
        result.append({
            "type": "library",
            "bom-ref": "pkg:golang/" + path + "@" + ver,
            "name": path,
            "version": ver,
        })
    return result


def build(root):
    root = os.path.abspath(root)
    comps = go_modules(root)
    if not comps:
        raise RuntimeError("no components parsed")
    doc = {
        "bomFormat": "CycloneDX",
        "specVersion": "1.4",
        "version": 1,
        "serialNumber": "urn:uuid:00000000-0000-4000-8000-%012x" % SER,
        "metadata": {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "tools": [{"vendor": "codvian-router", "name": "sbom.py"}],
            "component": {"type": "application", "name": "codvian-router"},
        },
        "components": comps,
    }
    return doc


def main(argv):
    root = os.path.dirname(os.path.abspath(__file__))
    root = os.path.dirname(root)  # repo root
    out = None
    selfcheck = False
    i = 0
    while i < len(argv):
        if argv[i] == "--out":
            out = argv[i + 1]
            i += 2
        elif argv[i] == "--self-check":
            selfcheck = True
            i += 1
        else:
            print("unknown option: " + argv[i], file=sys.stderr)
            return 2
    doc = build(root)
    text = json.dumps(doc, indent=2)
    if out:
        with open(out, "w") as f:
            f.write(text + "\n")
        print("sbom.py: wrote %d components to %s" % (len(doc["components"]), out))
    else:
        print(text)
    if selfcheck:
        json.loads(text)  # raises on malformed JSON
        print("sbom.py: self-check OK (%d components)" % len(doc["components"]))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))