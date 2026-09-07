import os
import unittest
from unittest.mock import Mock, patch

import release_check


class FakeGitHub:
    base = "repos/BrokkAi/release-bot"

    def __init__(self, runs=None, jobs=None):
        self.runs = runs or []
        self.jobs = jobs or []

    def api(self, path):
        if "/jobs?" in path:
            return {"jobs": self.jobs}
        return {"workflow_runs": self.runs}


class ReleaseChecks(unittest.TestCase):
    def test_selects_only_exact_nonpublishing_run(self):
        expected = {
            "id": 9, "head_sha": "a" * 40, "event": "workflow_dispatch",
            "path": release_check.RELEASE_WORKFLOW,
            "display_title": "Release v0.1.0 (publish=false)",
            "status": "completed", "conclusion": "success",
        }
        wrong_tag = dict(expected, id=10, display_title="Release v0.2.0 (publish=false)")
        selected = release_check.selected_preflight_run(FakeGitHub([expected, wrong_tag]), "a" * 40, "v0.1.0")
        self.assertEqual(selected["id"], 9)

    def test_incomplete_latest_preflight_is_not_hidden(self):
        complete = {
            "id": 9, "head_sha": "a" * 40, "event": "workflow_dispatch",
            "path": release_check.RELEASE_WORKFLOW,
            "display_title": "Release v0.1.0 (publish=false)",
            "status": "completed", "conclusion": "success",
        }
        incomplete = dict(complete, id=11, status="in_progress", conclusion=None)
        with self.assertRaisesRegex(ValueError, "did not complete"):
            release_check.selected_preflight_run(FakeGitHub([complete, incomplete]), "a" * 40, "v0.1.0")

    @patch.dict(os.environ, {"RELEASE_COMMIT": "a" * 40, "RELEASE_TAG": "v0.1.0", "RELEASE_TARGET": "b" * 40})
    def test_context_rejects_wrong_checkout(self):
        with patch.object(release_check.release, "commit", return_value="c" * 40):
            with self.assertRaisesRegex(ValueError, "HEAD"):
                release_check.context()

    def authorization_fixture(self, records=None, tag_sha=None):
        github = Mock(base="repos/fixture/repo")
        github.pages.return_value = records or []
        github.tag_commit.return_value = tag_sha
        run = {"id": 9, "head_sha": "a" * 40, "event": "workflow_dispatch",
               "path": release_check.RELEASE_WORKFLOW, "display_title": "Release v0.1.0 (publish=false)",
               "status": "completed", "conclusion": "success"}
        job = {"name": release_check.PUBLISHER_JOB, "status": "completed", "conclusion": "success",
               "steps": [{"name": release_check.PREFLIGHT_STEP, "status": "completed", "conclusion": "success"}]}
        github.api.side_effect = [{"workflow_runs": [run]}, {"jobs": [job]}]
        self.enterContext(patch.object(release_check, "context", return_value=("a" * 40, "v0.1.0")))
        self.enterContext(patch.object(release_check, "repository", return_value="fixture/repo"))
        self.enterContext(patch.object(release_check.release, "GitHub", return_value=github))
        return github, run, job

    def test_deleted_probe_draft_keeps_successful_workflow_evidence(self):
        github, _, _ = self.authorization_fixture()
        release_check.authorization()
        self.assertEqual(github.api.call_count, 2)
        self.assertTrue(all(len(call.args) == 1 for call in github.api.call_args_list))

    def test_preflight_skips_only_the_publication_dispatcher(self):
        github, run, job = self.authorization_fixture()
        skipped = {"name": release_check.DISPATCHER_JOB, "status": "completed", "conclusion": "skipped"}
        github.api.side_effect = [{"workflow_runs": [run]}, {"jobs": [job, skipped]}]
        release_check.authorization()
        for other in (dict(skipped, name="Tests"), dict(skipped, conclusion="failure")):
            github.api.side_effect = [{"workflow_runs": [run]}, {"jobs": [job, other]}]
            with self.assertRaises(ValueError):
                release_check.authorization()

    def test_published_release_uses_verification_without_preflight_or_writes(self):
        published = {"id": 1, "tag_name": "v0.1.0", "draft": False, "published_at": "2026-09-07"}
        github, _, _ = self.authorization_fixture([published], "a" * 40)
        with patch.object(release_check, "verify_published") as verify:
            release_check.authorization()
            verify.assert_called_once_with(github, published, "v0.1.0", "a" * 40)
        github.api.assert_not_called()
        with patch.object(release_check, "verify_published", side_effect=ValueError("corrupt download")):
            with self.assertRaisesRegex(ValueError, "corrupt download"):
                release_check.authorization()

    def test_conflicting_draft_and_tag_are_rejected(self):
        draft = {"id": 1, "tag_name": "v0.1.0", "draft": True, "target_commitish": "b" * 40}
        github, _, _ = self.authorization_fixture([draft])
        with self.assertRaisesRegex(ValueError, "draft targets another commit"):
            release_check.authorization()
        github.pages.return_value = []
        github.tag_commit.return_value = "b" * 40
        with self.assertRaisesRegex(ValueError, "another commit"):
            release_check.authorization()
        github.tag_commit.return_value = "a" * 40
        with self.assertRaisesRegex(ValueError, "without a recoverable"):
            release_check.authorization()

    def test_deleted_draft_does_not_bypass_failed_or_missing_authorization(self):
        github, run, job = self.authorization_fixture()
        for failure in ("run", "missing run", "job", "step", "missing job"):
            with self.subTest(failure=failure):
                failed_run = dict(run, conclusion="failure") if failure == "run" else run
                failed_job = dict(job)
                if failure == "job":
                    failed_job["conclusion"] = "failure"
                if failure == "step":
                    failed_job["steps"] = [dict(job["steps"][0], conclusion="failure")]
                github.api.side_effect = [
                    {"workflow_runs": [] if failure == "missing run" else [failed_run]},
                    {"jobs": [] if failure == "missing job" else [failed_job]},
                ]
                with self.assertRaises(ValueError):
                    release_check.authorization()


if __name__ == "__main__":
    unittest.main()
