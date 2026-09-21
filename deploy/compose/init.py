"""Generate persistent, local-only secrets and TLS material before Compose up."""

import base64
import json
import os
from pathlib import Path
import secrets
import subprocess


def write(path, content, mode=0o600):
    # Atomic replacement avoids leaving partial env/JSON files after interruption.
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(content)
    temporary.chmod(mode)
    temporary.replace(path)


def openssl(*args):
    result = subprocess.run(["openssl", *args], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError("OpenSSL failed: " + result.stderr.decode())


def main():
    os.umask(0o077)
    root = Path("/local")
    root.chmod(0o700)
    secrets_file = root / "secrets.json"
    if secrets_file.exists():
        values = json.loads(secrets_file.read_text())
    else:
        values = {
            "cookie_hash": base64.b64encode(secrets.token_bytes(64)).decode(),
            "cookie_block": base64.b64encode(secrets.token_bytes(32)).decode(),
            "oidc_secret": secrets.token_hex(32),
            "minio_password": secrets.token_hex(24),
            "admin_password": secrets.token_hex(24),
        }
        write(secrets_file, json.dumps(values, indent=2) + "\n")

    write(root / "gitone.env", "\n".join([
        "GITONE_COOKIE_HASH_KEY=" + values["cookie_hash"],
        "GITONE_COOKIE_BLOCK_KEY=" + values["cookie_block"],
        "GITONE_OIDC_CLIENT_SECRET=" + values["oidc_secret"],
        "AWS_ACCESS_KEY_ID=gitone-local",
        "AWS_SECRET_ACCESS_KEY=" + values["minio_password"],
    ]) + "\n")
    write(root / "minio.env", "\n".join([
        "MINIO_ROOT_USER=gitone-local",
        "MINIO_ROOT_PASSWORD=" + values["minio_password"],
        "AWS_ACCESS_KEY_ID=gitone-local",
        "AWS_SECRET_ACCESS_KEY=" + values["minio_password"],
    ]) + "\n")
    write(root / "keycloak.env", "\n".join([
        "KC_BOOTSTRAP_ADMIN_USERNAME=admin",
        "KC_BOOTSTRAP_ADMIN_PASSWORD=" + values["admin_password"],
    ]) + "\n")

    realm_dir = root / "keycloak"
    realm_dir.mkdir(exist_ok=True)
    realm_dir.chmod(0o755)  # Keycloak's non-root container reads the bind mount.
    realm = json.loads(Path("/tools/realm.json").read_text())
    realm["clients"][0]["secret"] = values["oidc_secret"]
    write(realm_dir / "gitone-realm.json", json.dumps(realm, indent=2) + "\n", 0o644)

    tls = root / "tls"
    tls.mkdir(exist_ok=True)
    tls.chmod(0o755)
    ca_key, ca_cert = tls / "ca.key", tls / "ca.crt"
    if not ca_key.exists() or not ca_cert.exists():
        if ca_key.exists() or ca_cert.exists():
            raise RuntimeError("Incomplete local CA; restore both ca.key and ca.crt from backup")
        openssl("req", "-x509", "-newkey", "rsa:3072", "-nodes", "-days", "3650",
                "-subj", "/CN=GitOne Local Development CA", "-keyout", str(ca_key),
                "-out", str(ca_cert), "-addext", "basicConstraints=critical,CA:TRUE",
                "-addext", "keyUsage=critical,keyCertSign,cRLSign")
        ca_cert.chmod(0o644)

    server_key, server_cert = tls / "server.key", tls / "server.crt"
    fresh = server_key.exists() and server_cert.exists() and subprocess.run(
        ["openssl", "x509", "-checkend", "604800", "-noout", "-in", str(server_cert)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode == 0
    if not fresh:
        extensions = tls / "server.ext"
        write(extensions, "\n".join([
            "basicConstraints=critical,CA:FALSE",
            "keyUsage=critical,digitalSignature,keyEncipherment",
            "extendedKeyUsage=serverAuth",
            "subjectAltName=DNS:gitone.localhost,DNS:keycloak.gitone.localhost,DNS:localhost,IP:127.0.0.1",
        ]) + "\n")
        openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=gitone.localhost",
                "-keyout", str(server_key), "-out", str(tls / "server.csr"))
        openssl("x509", "-req", "-days", "365", "-in", str(tls / "server.csr"),
                "-CA", str(ca_cert), "-CAkey", str(ca_key), "-CAcreateserial",
                "-extfile", str(extensions), "-out", str(server_cert))
        server_cert.chmod(0o644)
    print("Local secrets and TLS ready. Trust .local/tls/ca.crt in your browser (see deploy/compose/README.md).")


if __name__ == "__main__":
    main()
