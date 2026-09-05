#!/usr/bin/env python3
"""Only publish v-prefixed SemVer tags; branches and PRs need no version."""

import os
import re
import sys

# SemVer 2.0.0: numeric identifiers cannot have leading zeroes.
number = r"(?:0|[1-9][0-9]*)"
identifier = rf"(?:{number}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
version = rf"v{number}\.{number}\.{number}(?:-{identifier}(?:\.{identifier})*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
ref = os.environ.get("GITHUB_REF", "")
if ref.startswith("refs/tags/") and not re.fullmatch(version, ref.removeprefix("refs/tags/")):
    print("Release tag must be vMAJOR.MINOR.PATCH, optionally with SemVer prerelease/build metadata", file=sys.stderr)
    sys.exit(1)
