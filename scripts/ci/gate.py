#!/usr/bin/env python3
"""Fail closed unless every required CI job reports success."""

import json
import os
import sys


def check(results, required):
    if not required or not isinstance(results, dict) or set(results) != set(required):
        return ["CI dependencies are missing or do not match the required jobs"]
    failures = []
    for job in required:
        entry = results[job]
        status = entry.get("result") if isinstance(entry, dict) else None
        if status != "success":
            failures.append(f"{job}: {status or 'missing result'}")
    return failures


def main():
    try:
        results = json.loads(os.environ["CI_NEEDS"])
    except (KeyError, json.JSONDecodeError):
        print("CI gate failed: no valid dependency results", file=sys.stderr)
        return 1
    failures = check(results, sys.argv[1:])
    if failures:
        print("CI gate failed:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("CI gate passed: " + ", ".join(sys.argv[1:]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
