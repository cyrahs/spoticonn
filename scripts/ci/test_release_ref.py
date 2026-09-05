import os
from pathlib import Path
import subprocess
import sys
import unittest


class ReleaseRefTests(unittest.TestCase):
    def run_check(self, ref):
        return subprocess.run(
            [sys.executable, str(Path(__file__).with_name("check-release-ref.py"))],
            env={**os.environ, "GITHUB_REF": ref}, capture_output=True, text=True,
        ).returncode

    def test_supported_refs(self):
        for ref in ["refs/heads/main", "refs/pull/12/merge", "refs/tags/v0.1.0",
                    "refs/tags/v1.2.3-rc.1", "refs/tags/v1.2.3+build.5"]:
            with self.subTest(ref=ref):
                self.assertEqual(self.run_check(ref), 0)

    def test_invalid_version_tags_cannot_publish(self):
        for tag in ["v1", "v1.2", "vlatest", "v01.2.3", "v1.2.3-01", "v1.2.3/extra", "v1.2.3-"]:
            with self.subTest(tag=tag):
                self.assertNotEqual(self.run_check("refs/tags/" + tag), 0)


if __name__ == "__main__":
    unittest.main()
