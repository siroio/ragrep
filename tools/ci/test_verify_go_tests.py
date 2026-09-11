import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from verify_go_tests import verify


def event(action, test, package="example"):
    return json.dumps({"Action": action, "Package": package, "Test": test})


REQUIRED = ["TestEnsureAssets", "TestEmbedProperties", "TestSmoke", "TestSmokeMCP"]


class VerifyTests(unittest.TestCase):
    def test_requires_each_named_test_to_pass(self):
        stream = "\n".join(event(action, name) for name in REQUIRED for action in ("run", "pass"))
        self.assertEqual(verify(stream.splitlines(), REQUIRED), [])

    def test_skip_is_a_failure(self):
        lines = [event("run", name) for name in REQUIRED] + [event("skip", "TestSmoke")]
        lines += [event("pass", name) for name in REQUIRED if name != "TestSmoke"]
        self.assertIn("TestSmoke: skipped", verify(lines, REQUIRED))

    def test_missing_and_failed_tests_are_reported(self):
        lines = [event("run", name) for name in REQUIRED if name != "TestSmokeMCP"]
        lines += [event("pass", name) for name in REQUIRED if name not in ("TestSmoke", "TestSmokeMCP")]
        lines += [event("fail", "TestSmoke")]
        errors = verify(lines, REQUIRED)
        self.assertIn("TestSmoke: failed", errors)
        self.assertIn("TestSmokeMCP: missing", errors)


if __name__ == "__main__":
    unittest.main()
