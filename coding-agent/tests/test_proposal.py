import hashlib
import io
import unittest
import zipfile

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
        cases = [{}, {"../file": b"x"}, {".github/workflow": b"x"},
                 {".env.local": b"x"}, {"test": b"AKIAexample"},
                 {"test": b"\xff"}, {"test": b"\0"}, {"test": b"x" * (128 * 1024 + 1)}]
        for files in cases:
            with self.subTest(files=list(files)), self.assertRaises(ValueError):
                create_proposal(files)


if __name__ == "__main__":
    unittest.main()
