import unittest
from unittest.mock import Mock, patch

import package_check as check


class EvidenceTests(unittest.TestCase):
    def test_wrong_sha_failed_runs_missing_steps_and_expired_artifacts_fail(self):
        sha, tag = "a" * 40, "v0.6.0"
        run = dict(id=5, head_sha=sha, event="workflow_dispatch", path=check.WORKFLOW,
                   display_title=f"Release {tag} (publish=false)", status="completed", conclusion="success")
        job = dict(name="packages", status="completed", conclusion="success", steps=[dict(name=check.AUTH_STEP, status="completed", conclusion="success")])
        artifact = dict(id=6, name=f"packages-{tag}", expired=False, size_in_bytes=100, workflow_run=dict(head_sha=sha))
        with patch.object(check.release_check, "context", return_value=(sha, tag)), patch.object(check.release_check, "repository", return_value="BrokkAi/release-bot"), patch.object(check.release, "GitHub") as cls:
            github = cls.return_value
            github.base = "repos/BrokkAi/release-bot"
            for selected, jobs, artifacts, success in [
                (run, [job], [artifact], True),
                (dict(run, head_sha="b" * 40), [job], [artifact], False),
                (dict(run, conclusion="failure"), [job], [artifact], False),
                (run, [dict(job, steps=[])], [artifact], False),
                (run, [job], [dict(artifact, expired=True)], False),
                (run, [job], [], False),
            ]:
                github.api.side_effect = [{"workflow_runs": [selected]}, {"jobs": jobs}, {"artifacts": artifacts}]
                if success:
                    check.evidence(check.AUTH_STEP)
                else:
                    with self.assertRaises(ValueError):
                        check.evidence(check.AUTH_STEP)
