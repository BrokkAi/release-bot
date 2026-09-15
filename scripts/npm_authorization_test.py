import datetime
import os
import unittest
from unittest.mock import patch

import npm_authorization as auth


class AuthorizationTests(unittest.TestCase):
    def test_exchange_accepts_numeric_unix_expiry(self):
        expiry = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)
        self.assertEqual(auth.exchange_expiry(int(expiry.timestamp())).timestamp(), int(expiry.timestamp()))
        with self.assertRaisesRegex(ValueError, 'unsupported expiry'):
            auth.exchange_expiry(True)

    @patch.dict(os.environ, {"GITHUB_REPOSITORY": "BrokkAi/release-bot", "NODE_AUTH_TOKEN": "", "ACTIONS_ID_TOKEN_REQUEST_URL": "https://example.test?x=y", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "secret"})
    def test_checks_every_package_and_rejects_expired_tokens(self):
        future = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)).isoformat()
        token = {"token_type": "oidc", "token": "private", "expires": future}
        with patch.object(auth, "request_json", side_effect=[{"value": "identity"}] + [token] * 5) as request:
            auth.check()
            self.assertEqual(request.call_count, 6)
            self.assertTrue(all(c.args[2] == "POST" for c in request.call_args_list[1:]))
        token["expires"] = "2000-01-01T00:00:00Z"
        with patch.object(auth, "request_json", side_effect=[{"value": "identity"}, token]):
            with self.assertRaisesRegex(ValueError, "expiring"):
                auth.check()

    @patch.dict(os.environ, {"NODE_AUTH_TOKEN": "different-publisher"})
    def test_cannot_substitute_oidc_for_configured_token(self):
        with patch.object(auth, "request_json") as request:
            with self.assertRaisesRegex(ValueError, "different publishing identity"):
                auth.check()
            request.assert_not_called()
