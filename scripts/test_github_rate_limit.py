import importlib.util
import io
from email.message import Message
from pathlib import Path
import unittest
import urllib.error
from unittest.mock import Mock, patch


spec = importlib.util.spec_from_file_location('rate_limit_admit', Path(__file__).with_name('admit-installation.py'))
admit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(admit)


class GitHubRateLimitTests(unittest.TestCase):
    def setUp(self):
        self.request = {'user': {'id': 7, 'login': 'owner'}, 'account': {'id': 10, 'login': 'example', 'type': 'Organization'}}

    def opener_error(self, code, headers, body=b'private upstream details'):
        response_headers = Message()
        for key, value in headers.items():
            response_headers[key] = value
        error = urllib.error.HTTPError('https://api.github.com/orgs/example/memberships/owner', code, 'fixture', response_headers, io.BytesIO(body))
        opener = Mock()
        opener.open.side_effect = error
        return patch.object(admit.setup.urllib.request, 'build_opener', return_value=opener)

    def test_rate_limit_propagates_unavailable_instead_of_revoking_owner(self):
        for code, headers in [(403, {'X-RateLimit-Remaining': '0'}), (403, {'Retry-After': '60'}), (429, {})]:
            with self.subTest(code=code, headers=headers), self.opener_error(code, headers):
                with self.assertRaises(RuntimeError) as caught:
                    admit.current_organization_owner(self.request, 'fixture-token')
                self.assertNotEqual(str(caught.exception), 'GitHub setup API returned HTTP 403')
                self.assertNotIn('private upstream details', str(caught.exception))

    def test_secondary_rate_limit_without_optional_headers_remains_unavailable(self):
        with self.opener_error(403, {}, b'{"message":"You have exceeded a secondary rate limit"}'):
            with self.assertRaisesRegex(RuntimeError, 'temporarily rate limited'):
                admit.current_organization_owner(self.request, 'fixture-token')

    def test_normal_forbidden_membership_is_denied(self):
        with self.opener_error(403, {}):
            self.assertFalse(admit.current_organization_owner(self.request, 'fixture-token'))


if __name__ == '__main__':
    unittest.main()
