"""Regression tests for the release label merge gate."""

from __future__ import annotations

import io
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

from scripts.check_release_labels import main


class ReleaseLabelTests(unittest.TestCase):
    """Exercise ordinary PRs and the trusted generated-release exemption."""

    def check_labels(
        self,
        labels_json: str,
        *,
        author: str = "contributor",
        head_ref: str = "feature/example",
        head_repository: str = "opsmill/infrahub-backup",
        title: str = "Example change",
    ) -> tuple[int, str, str]:
        """Run the checker with a representative pull-request payload."""
        args = [
            "check_release_labels.py",
            "--labels-json",
            labels_json,
            "--author-login",
            author,
            "--head-ref",
            head_ref,
            "--head-repository",
            head_repository,
            "--repository",
            "opsmill/infrahub-backup",
            f"--title={title}",
        ]
        stdout = io.StringIO()
        stderr = io.StringIO()
        with patch("sys.argv", args), redirect_stdout(stdout), redirect_stderr(stderr):
            result = main()
        return result, stdout.getvalue(), stderr.getvalue()

    def test_one_bump_label_passes(self) -> None:
        result, output, error = self.check_labels('["changes/patch"]')
        self.assertEqual(result, 0)
        self.assertIn("changes/patch", output)
        self.assertEqual(error, "")

    def test_no_bump_label_fails(self) -> None:
        result, _, error = self.check_labels("[]")
        self.assertEqual(result, 1)
        self.assertIn("found: none", error)

    def test_multiple_bump_labels_fail(self) -> None:
        result, _, error = self.check_labels('["changes/patch", "changes/minor"]')
        self.assertEqual(result, 1)
        self.assertIn("changes/minor, changes/patch", error)

    def test_generated_release_pr_is_exempt(self) -> None:
        result, output, error = self.check_labels(
            "[]", author="opsmill-bot", head_ref="release/v1.2.3", title="chore(release): v1.2.3"
        )
        self.assertEqual(result, 0)
        self.assertIn("Skipping label check", output)
        self.assertEqual(error, "")

    def test_similar_human_pr_is_not_exempt(self) -> None:
        result, _, error = self.check_labels("[]", head_ref="release/v1.2.3", title="chore(release): v1.2.3")
        self.assertEqual(result, 1)
        self.assertIn("found: none", error)

    def test_forked_release_pr_is_not_exempt(self) -> None:
        result, _, error = self.check_labels(
            "[]",
            author="opsmill-bot",
            head_ref="release/v1.2.3",
            head_repository="someone/infrahub-backup",
            title="chore(release): v1.2.3",
        )
        self.assertEqual(result, 1)
        self.assertIn("found: none", error)


if __name__ == "__main__":
    unittest.main()
