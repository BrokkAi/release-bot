import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import release_pipeline as pipeline


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = self.enterContext(tempfile.TemporaryDirectory())
        self.root = Path(self.temp)
        (self.root / "npm").mkdir()
        (self.root / "npm/manifest.json").write_text(json.dumps({"tag": "v0.6.0", "commit": "a" * 40}))
        self.enterContext(patch.dict(os.environ, {"GH_REPO": "BrokkAi/release-bot"}))
        self.enterContext(patch.object(pipeline.release, "commit", return_value="a" * 40))
        self.enterContext(patch.object(pipeline.release, "verify_local"))
        self.github = Mock()
        self.github.prepare.return_value = {"draft": True}
        self.enterContext(patch.object(pipeline.release, "GitHub", return_value=self.github))
        self.auth = self.enterContext(patch.object(pipeline.npm_authorization, "check"))
        self.registry = self.enterContext(patch.object(pipeline.package_registry, "run"))

    def test_preflight_never_uploads(self):
        pipeline.run("preflight", "v0.6.0", self.root, self.root)
        self.registry.assert_called_once_with("check", self.root, "npm")
        self.github.publish.assert_not_called()

    def test_authorization_failure_prevents_uploads(self):
        self.auth.side_effect = ValueError("denied")
        with self.assertRaisesRegex(ValueError, "denied"):
            pipeline.run("publish", "v0.6.0", self.root, self.root)
        self.registry.assert_called_once_with("check", self.root, "npm")
        self.github.publish.assert_not_called()

    def test_npm_failure_prevents_github_finalization(self):
        self.registry.side_effect = [None, ValueError("conflicting npm")]
        with self.assertRaisesRegex(ValueError, "conflicting"):
            pipeline.run("publish", "v0.6.0", self.root, self.root)
        self.github.publish.assert_not_called()

    def test_github_finalizes_only_after_npm_verification(self):
        events = []
        self.registry.side_effect = lambda command, *args: events.append(command)
        self.github.publish.side_effect = lambda *args: events.append("github")
        pipeline.run("publish", "v0.6.0", self.root, self.root)
        self.assertEqual(events, ["check", "publish", "verify", "github"])
