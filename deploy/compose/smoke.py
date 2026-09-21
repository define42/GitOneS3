"""Exercise real Keycloak code flow, shard forwarding, MinIO and group access."""

from html.parser import HTMLParser
from http.cookiejar import CookieJar
import json
import secrets
import ssl
import urllib.error
import urllib.parse
import urllib.request


ORIGIN = "https://gitone.localhost:8443"
ISSUER = "https://keycloak.gitone.localhost:8443/realms/gitone"
TLS = ssl.create_default_context(cafile="/local/ca.crt")


class LoginForm(HTMLParser):
    def __init__(self):
        super().__init__()
        self.action = None
        self.fields = {}

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "form" and attrs.get("id") == "kc-form-login":
            self.action = attrs.get("action")
        if tag == "input" and attrs.get("type") == "hidden" and attrs.get("name"):
            self.fields[attrs["name"]] = attrs.get("value", "")


def browser():
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=TLS),
        urllib.request.HTTPCookieProcessor(CookieJar()),
    )


def request(client, path, method="GET", payload=None, csrf=None, expected=200):
    headers = {}
    body = None
    if method != "GET":
        headers["Origin"] = ORIGIN
        headers["X-CSRF-Token"] = csrf or ""
        body = b""
    if payload is not None:
        body = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(ORIGIN + path, data=body, headers=headers, method=method)
    try:
        response = client.open(req, timeout=30)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        data = response.read()
        if response.status != expected:
            raise RuntimeError(f"{method} {path}: expected {expected}, got {response.status}")
        if "application/json" in response.headers.get("Content-Type", ""):
            return json.loads(data)
        return data


def login(namespace, username):
    client = browser()
    form = LoginForm()
    form.feed(request(client, f"/{namespace}/auth/oidc/login").decode())
    if not form.action or not form.action.startswith(ISSUER + "/"):
        raise RuntimeError("Login did not reach the configured Keycloak realm")
    form.fields.update(username=username, password=username + "-dev-password", credentialId="")
    req = urllib.request.Request(form.action, data=urllib.parse.urlencode(form.fields).encode())
    with client.open(req, timeout=30) as response:
        if response.geturl() != ORIGIN + "/" + namespace:
            raise RuntimeError("Keycloak did not finish the authorization code callback")
        session = json.load(response)
    assert session["identity"]["issuer"] == ISSUER
    assert session["userId"].startswith("oidc:")
    return client, session


def main():
    probe = browser()
    for shard in range(4):
        host = f"gitone-{shard}.gitone-headless.local.svc"
        with probe.open(f"http://{host}:8080/readyz", timeout=10) as response:
            assert response.status == 200
    with probe.open(ISSUER + "/.well-known/openid-configuration", timeout=10) as response:
        assert json.load(response)["issuer"] == ISSUER

    suffix = secrets.token_hex(6)
    alice, alice_session = login("smoke-alice-" + suffix, "alice")
    bob, bob_session = login("smoke-bob-" + suffix, "bob")
    # Repeated requests pass through the round-robin entry point. The cookie
    # works on every entry instance and the owner remains authoritative.
    for _ in range(8):
        result = request(alice, "/smoke-alice-" + suffix + "/auth/session")
        assert result["userId"] == alice_session["userId"]

    group = "/smoke-group-" + suffix
    owner_csrf = alice_session["csrfToken"]
    member_csrf = bob_session["csrfToken"]
    created = request(alice, group, "POST", csrf=owner_csrf, expected=201)
    assert created["creatorUserId"] == alice_session["userId"]
    request(bob, group, expected=403)
    request(alice, group + "/invitations", "POST",
            {"userId": bob_session["userId"], "role": "reader"}, owner_csrf)
    request(bob, group, expected=403)
    request(bob, group + "/invitations/accept", "POST", csrf=member_csrf)
    assert request(bob, group)["role"] == "reader"
    request(bob, group + "/example.git/git-receive-pack", "POST", csrf=member_csrf, expected=403)
    request(alice, group + "/members", "DELETE", {"userId": bob_session["userId"]}, owner_csrf)
    request(bob, group, expected=403)
    print("PASS: four shards ready; real Keycloak login, shared cookies, group invitation/acceptance and revocation")


if __name__ == "__main__":
    main()
