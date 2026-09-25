"""Real HTTPS Git + Keycloak + four-shard MinIO smoke test, without secret output.

Run from the source checkout after make run:
    python3 deploy/compose/git_smoke.py
Requires Python 3.10+ and native Git; only the public local CA is read.
Created namespaces/repositories persist; created PATs are revoked on cleanup.
"""

import base64
from html.parser import HTMLParser
import http.client
from http.cookiejar import CookieJar
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request


ORIGIN = "https://gitone.localhost:8443"
ISSUER = "https://keycloak.gitone.localhost:8443/realms/gitone"
ROOT = Path(__file__).resolve().parents[2]
CA_FILE = ROOT / ".local/tls/ca.crt"


class SmokeFailure(Exception):
    """A diagnostic whose text is safe to print without credentials or state."""


def require(condition, message):
    if not condition:
        raise SmokeFailure(message)


def shard_for_short_name(name):
    """Seed-zero XXH64 for fixture names shorter than 32 bytes, modulo four."""
    data = name.encode("ascii")
    require(len(data) < 32, "Fixture name exceeds the short XXH64 implementation")
    mask = (1 << 64) - 1
    p1, p2, p3 = 11400714785074694791, 14029467366897019727, 1609587929392839161
    p4, p5 = 9650029242287828579, 2870177450012600261

    def rotate(value, count):
        value &= mask
        return ((value << count) | (value >> (64 - count))) & mask

    value = p5 + len(data)
    while len(data) >= 8:
        lane = rotate(int.from_bytes(data[:8], "little") * p2, 31) * p1 & mask
        value = (rotate(value ^ lane, 27) * p1 + p4) & mask
        data = data[8:]
    if len(data) >= 4:
        value = (rotate(value ^ (int.from_bytes(data[:4], "little") * p1), 23) * p2 + p3) & mask
        data = data[4:]
    for byte in data:
        value = rotate(value ^ (byte * p5), 11) * p1 & mask
    value ^= value >> 33
    value = value * p2 & mask
    value ^= value >> 29
    value = value * p3 & mask
    value ^= value >> 32
    return value % 4


class LoopbackHTTPSConnection(http.client.HTTPSConnection):
    def connect(self):
        # Keep real hostname/SNI/certificate checks, but avoid hosts-file edits.
        require(self.host in {"gitone.localhost", "keycloak.gitone.localhost"}
                and self.port == 8443, "Refusing a non-local smoke-test destination")
        self.sock = socket.create_connection(("127.0.0.1", 8443), self.timeout)
        self.sock = self._context.wrap_socket(self.sock, server_hostname=self.host)


class LoopbackHTTPSHandler(urllib.request.HTTPSHandler):
    def https_open(self, request):
        return self.do_open(LoopbackHTTPSConnection, request, context=self._context)


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


class Smoke:
    def __init__(self):
        require(shutil.which("git") is not None, "Native Git is required")
        require(CA_FILE.is_file(), "Local CA missing; run make run first")
        self.tls = ssl.create_default_context(cafile=str(CA_FILE))
        self.tokens = []

    def browser(self):
        return urllib.request.build_opener(
            urllib.request.ProxyHandler({}), LoopbackHTTPSHandler(context=self.tls),
            urllib.request.HTTPCookieProcessor(CookieJar()),
        )

    def request(self, client, path, method="GET", payload=None, csrf=None, expected=200, credential=None):
        headers = {"Accept": "application/json"}
        body = None
        if method != "GET":
            headers.update(Origin=ORIGIN, **{"X-CSRF-Token": csrf or ""})
            body = b""
        if payload is not None:
            body = json.dumps(payload).encode()
            headers["Content-Type"] = "application/json"
        if credential:
            headers["Authorization"] = "Basic " + base64.b64encode(
                (credential[0] + ":" + credential[1]).encode()).decode()
        req = urllib.request.Request(ORIGIN + path, data=body, headers=headers, method=method)
        try:
            response = client.open(req, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        except (OSError, urllib.error.URLError):
            raise SmokeFailure("HTTPS request failed; check stack readiness and local CA") from None
        with response:
            data = response.read(2 << 20)
            require(response.status == expected,
                    f"{method} {path}: expected HTTP {expected}, got {response.status}")
            if "application/json" in response.headers.get("Content-Type", ""):
                return json.loads(data)
            return data

    def login(self, namespace, account):
        client = self.browser()
        form = LoginForm()
        form.feed(self.request(client, f"/{namespace}/auth/oidc/login").decode())
        require(form.action and form.action.startswith(ISSUER + "/"),
                "Login did not reach the configured Keycloak realm")
        form.fields.update(username=account, password=account + "-dev-password", credentialId="")
        request = urllib.request.Request(form.action, data=urllib.parse.urlencode(form.fields).encode())
        try:
            with client.open(request, timeout=30) as response:
                require(response.geturl() == ORIGIN + "/" + namespace,
                        "Keycloak did not finish the authorization-code callback")
                session = json.load(response)
        except (OSError, urllib.error.URLError):
            raise SmokeFailure("Keycloak login failed; credentials and callback details suppressed") from None
        require(session["identity"]["issuer"] == ISSUER, "Unexpected verified OIDC issuer")
        require(session["userId"].startswith("oidc:"), "Unexpected verified account identity")
        return {"client": client, "name": namespace, "csrf": session["csrfToken"], "id": session["userId"]}

    def api(self, user, path, method="GET", payload=None, expected=200):
        return self.request(user["client"], "/api/v1" + path, method, payload, user["csrf"], expected)

    def token(self, user, permission, repositories):
        created = self.api(user, f"/users/{user['name']}/tokens", "POST", {
            "name": "Compose native-Git smoke", "permission": permission,
            "repositories": repositories, "expiresInDays": 1,
        }, 201)
        token = {"user": user, "id": created["metadata"]["id"], "secret": created["token"]}
        self.tokens.append(token)
        return token

    def revoke(self, token):
        self.api(token["user"], f"/users/{token['user']['name']}/tokens/{token['id']}", "DELETE", expected=204)

    def git(self, directory, token, *arguments, denied=False):
        # The secret is only in the child environment, never an argv/URL/file.
        helper = "!f() { test \"$1\" = get || exit 0; printf 'username=%s\\npassword=%s\\n' \"$GITONE_SMOKE_USERNAME\" \"$GITONE_SMOKE_PAT\"; }; f"
        options = ["-c", "credential.helper=", "-c", "credential.helper=" + helper,
                   "-c", "http.sslCAInfo=" + str(CA_FILE),
                   "-c", "http.curloptResolve=gitone.localhost:8443:127.0.0.1",
                   "-c", "user.name=GitOne smoke", "-c", "user.email=smoke@users.gitone.invalid",
                   "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main"]
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith(("GIT_", "GCM_", "GITONE_SMOKE_"))}
        environment.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                           GIT_TERMINAL_PROMPT="0", GIT_ASKPASS="", GITONE_SMOKE_USERNAME=token["user"]["name"],
                           GITONE_SMOKE_PAT=token["secret"], NO_PROXY=".localhost,127.0.0.1",
                           no_proxy=".localhost,127.0.0.1")
        try:
            result = subprocess.run(["git", *options, *arguments], cwd=directory, env=environment,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60, check=False)
        except subprocess.TimeoutExpired:
            raise SmokeFailure(f"git {arguments[0]} timed out; output suppressed") from None
        require((result.returncode != 0) if denied else (result.returncode == 0),
                f"git {arguments[0]} {'unexpectedly succeeded' if denied else 'failed'}; output suppressed")
        return result.stdout.decode()

    def commit(self, checkout, token, content):
        (checkout / "README.md").write_text(content)
        self.git(checkout, token, "add", "README.md")
        self.git(checkout, token, "commit", "-m", "Compose native Git smoke")

    def blob(self, user, repository, content):
        actual = self.api(user, f"/repos/{repository}/blob?ref=main&path=README.md")
        require(actual["content"] == content and not actual["binary"], "Browser blob disagrees with pushed Git content")

    def run(self):
        suffix = secrets.token_hex(5)
        alice = self.login("git-a-" + suffix, "alice")
        bob = self.login("git-b-" + suffix, "bob")
        session = self.api(alice, "/session")
        require(session["shardCount"] == 4, "This smoke test requires the four-shard Compose stack")
        group = next(f"git-g-{suffix}-{index}" for index in range(100)
                     if shard_for_short_name(f"git-g-{suffix}-{index}") != shard_for_short_name(alice["name"]))
        personal = alice["name"] + "/project"
        empty = alice["name"] + "/empty"
        shared = group + "/shared"
        self.api(alice, "/repos/" + alice["name"], "POST", {"name": "project", "initializeReadme": True}, 201)
        self.api(alice, "/repos/" + alice["name"], "POST", {"name": "empty"}, 201)
        write = self.token(alice, "write", [personal, empty])
        read = self.token(alice, "read", [personal])
        with tempfile.TemporaryDirectory(prefix="gitone-git-smoke-") as temporary:
            work = Path(temporary)
            writer, reader = work / "writer", work / "reader"
            self.git(work, write, "clone", ORIGIN + "/" + personal + ".git", str(writer))
            self.git(work, read, "clone", ORIGIN + "/" + personal + ".git", str(reader))
            content = "# project\n\nReal HTTPS Git, Keycloak, four shards, and MinIO.\n"
            self.commit(writer, write, content)
            self.git(writer, write, "push", "origin", "HEAD:main")
            self.git(reader, read, "pull", "--ff-only", "origin", "main")
            require((reader / "README.md").read_text() == content, "Pulled README did not match")
            self.blob(alice, personal, content)
            history = self.api(alice, f"/repos/{personal}/commits?ref=main")
            require(history["commits"][0]["id"] == self.git(writer, write, "rev-parse", "HEAD").strip(),
                    "Browser history disagrees with pushed Git commit")
            self.commit(reader, read, "This push must be denied.\n")
            self.git(reader, read, "push", "origin", "HEAD:main", denied=True)
            self.blob(alice, personal, content)
            first = work / "empty"
            self.git(work, write, "clone", ORIGIN + "/" + empty + ".git", str(first))
            self.commit(first, write, "# First push to an empty repository\n")
            self.git(first, write, "push", "origin", "HEAD:main")
            self.blob(alice, empty, "# First push to an empty repository\n")
            self.revoke(write)
            self.git(writer, write, "fetch", "origin", denied=True)

            self.api(bob, "/groups/" + group, "POST", expected=201)
            self.api(bob, f"/groups/{group}/invitations", "POST", {"userId": alice["id"], "role": "developer"})
            self.api(alice, f"/groups/{group}/invitations/accept", "POST")
            self.api(alice, "/repos/" + group, "POST", {"name": "shared", "initializeReadme": True}, 201)
            group_token = self.token(alice, "write", [shared])
            checkout = work / "shared"
            self.git(work, group_token, "clone", ORIGIN + "/" + shared + ".git", str(checkout))
            self.commit(checkout, group_token, "# Cross-shard group push\n")
            self.git(checkout, group_token, "push", "origin", "HEAD:main")
            self.blob(bob, shared, "# Cross-shard group push\n")
            self.api(bob, f"/groups/{group}/members", "DELETE", {"userId": alice["id"]})
            self.request(self.browser(), "/" + shared + ".git/info/refs?service=git-upload-pack",
                         expected=403, credential=(alice["name"], group_token["secret"]))
            self.git(checkout, group_token, "fetch", "origin", denied=True)
        print("PASS: real HTTPS clone/push/pull, empty first push, MinIO/browser agreement, read-only and revoked PATs, cross-shard group revocation")

    def cleanup(self):
        complete = True
        for token in self.tokens:
            try:
                self.revoke(token)
            except Exception:
                complete = False
        if not complete:
            print("WARNING: token cleanup incomplete; revoke Compose native-Git smoke tokens in the UI (one-day expiry)", file=sys.stderr)


def main():
    for value, expected in {"": 1, "a": 3, "abc": 1, "message digest": 2,
                            "abcdefghijklmnopqrstuvwxyz": 0}.items():
        require(shard_for_short_name(value) == expected, "Fixture XXH64 routing self-test failed")
    if sys.argv[1:] == ["--self-test"]:
        print("PASS: fixture routing self-test")
        return
    require(not sys.argv[1:], "Usage: python3 deploy/compose/git_smoke.py [--self-test]")
    smoke = Smoke()
    try:
        smoke.run()
    finally:
        smoke.cleanup()


if __name__ == "__main__":
    try:
        main()
    except SmokeFailure as error:
        print("FAIL: " + str(error), file=sys.stderr)
        sys.exit(1)
    except Exception as error:
        # OIDC/library exception strings can contain callback parameters; no traceback.
        print("FAIL: " + type(error).__name__ + "; details suppressed to protect credentials", file=sys.stderr)
        sys.exit(1)
