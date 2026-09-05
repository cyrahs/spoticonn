import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

from gate import check


class GateTests(unittest.TestCase):
    required = ["configuration", "go-quality", "go-tests", "frontend", "binary", "image"]

    def successful(self):
        return {job: {"result": "success"} for job in self.required}

    def test_all_modules_must_succeed(self):
        self.assertEqual(check(self.successful(), self.required), [])

    def test_failure_cancellation_and_skips_block_every_module(self):
        for job in self.required:
            for status in ["failure", "cancelled", "skipped", "pending", None]:
                with self.subTest(job=job, status=status):
                    results = copy.deepcopy(self.successful())
                    results[job]["result"] = status
                    self.assertTrue(check(results, self.required))

    def test_missing_or_malformed_dependencies_cannot_pass(self):
        for results in [{}, [], None, {"frontend": {"result": "success"}}]:
            self.assertTrue(check(results, self.required))
        for value in [{}, None, "success"]:
            results = self.successful()
            results["image"] = value
            self.assertTrue(check(results, self.required))
        self.assertTrue(check(self.successful(), []))
        self.assertTrue(check(self.successful(), self.required + ["new-module"]))

    def test_cli_returns_failure_for_invalid_input(self):
        script = Path(__file__).with_name("gate.py")
        for value, expected in [("not json", 1), ("{}", 1), (json.dumps(self.successful()), 0)]:
            with self.subTest(value=value):
                result = subprocess.run(
                    [sys.executable, str(script), *self.required],
                    env={**os.environ, "CI_NEEDS": value}, capture_output=True, text=True,
                )
                self.assertEqual(result.returncode, expected, result.stderr)


if __name__ == "__main__":
    unittest.main()
