#!/usr/bin/env python3
"""Check package-scoped npm OIDC exchanges without uploading a package.

Uses npm's documented package-scoped token exchange API:
https://api-docs.npmjs.com/#tag/OIDC
Tokens stay in memory and are never logged or saved.
"""
import datetime
import json
import os
import urllib.error
import urllib.parse
import urllib.request

import package_installers

PACKAGES = tuple(f"{package_installers.NPM_ROOT}-{system}-{arch}"
                 for system in ("linux", "darwin") for arch in ("x64", "arm64")) + (package_installers.NPM_ROOT,)


def exchange_expiry(value):
    if isinstance(value, int) and not isinstance(value, bool):
        return datetime.datetime.fromtimestamp(value, datetime.timezone.utc)
    if isinstance(value, str):
        return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    raise ValueError("npm exchange returned an unsupported expiry")


def request_json(url, token, method="GET"):
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}",
                                                  "Accept": "application/json"}, method=method)
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        # Do not include response bodies or request headers in errors.
        raise ValueError(f"Publisher authorization failed: HTTP {error.code} at {urllib.parse.urlsplit(url).path}") from None


def check():
    if os.environ.get("NODE_AUTH_TOKEN"):
        raise ValueError("NPM_TOKEN is configured: OIDC evidence cannot validate that different publishing identity. Remove the legacy environment secret to use the documented trusted publishers.")
    if os.environ.get("GITHUB_REPOSITORY") != "BrokkAi/release-bot":
        raise ValueError("publisher check must execute in BrokkAi/release-bot Actions")
    url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"]
    url += ("&" if "?" in url else "?") + urllib.parse.urlencode({"audience": "npm:registry.npmjs.org"})
    identity = request_json(url, os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])["value"]
    for package in PACKAGES:
        endpoint = "https://registry.npmjs.org/-/npm/v1/oidc/token/exchange/package/" + urllib.parse.quote(package, safe="")
        result = request_json(endpoint, identity, "POST")
        expires = exchange_expiry(result["expires"])
        if result.get("token_type") != "oidc" or not result.get("token") or expires <= datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(minutes=5):
            raise ValueError(f"Missing or expiring package-scoped OIDC token for {package}")
        del result
        print(f"npm accepted package-scoped OIDC exchange for {package}; token expires {expires.isoformat()}")


if __name__ == "__main__":
    check()
