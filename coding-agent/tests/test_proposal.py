import hashlib
import io
import unittest
import zipfile
from unittest.mock import patch

from hako_coding_agent.proposal import create_proposal


class ProposalTest(unittest.TestCase):
    def test_sorted_deterministic_archive(self):
        first = create_proposal({"z.py": b"z\n", "a.py": b"a\n"})
        second = create_proposal({"a.py": b"a\n", "z.py": b"z\n"})
        self.assertEqual(first, second)
        self.assertEqual(first.sha256, hashlib.sha256(first.data).hexdigest())
        with zipfile.ZipFile(io.BytesIO(first.data)) as archive:
            self.assertEqual(archive.namelist(), ["a.py", "z.py"])
            self.assertEqual(archive.read("a.py"), b"a\n")

    def test_rejects_unsafe_files(self):
        cases = [
            {},
            {"../file": b"x"},
            {"/absolute": b"x"},
            {"a//b": b"x"},
            {"a\\b": b"x"},
            {"a:b": b"x"},
            {".github/workflow": b"x"},
            {"infra/main.tf": b"x"},
            {".env.local": b"x"},
            {"cert.pem": b"x"},
            {"credentials": b"x"},
            {"test": b"AKIAexample"},
            {"test": b"-----BEGIN PRIVATE KEY-----"},
            {"test": b"sk-ant-api-example"},
            {"test": b"\xff"},
            {"test": b"\0"},
            {"test": b"x" * (128 * 1024 + 1)},
        ]
        for files in cases:
            with self.subTest(files=list(files)), self.assertRaises(ValueError):
                create_proposal(files)

    def test_enforces_proposal_count_content_and_archive_limits(self):
        with patch("hako_coding_agent.proposal.MAX_FILES", 1):
            with self.assertRaisesRegex(ValueError, "file count"):
                create_proposal({"a": b"a", "b": b"b"})
        with patch("hako_coding_agent.proposal.MAX_FILE_BYTES", 1):
            with self.assertRaisesRegex(ValueError, "content"):
                create_proposal({"a": b"ab"})
        with patch("hako_coding_agent.proposal.MAX_ARCHIVE_BYTES", 10):
            with self.assertRaisesRegex(ValueError, "archive too large"):
                create_proposal({"a": b"a"})


if __name__ == "__main__":
    unittest.main()
