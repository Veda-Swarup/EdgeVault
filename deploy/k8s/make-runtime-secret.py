#!/usr/bin/env python3
"""Create the namespace runtime secret without printing credentials."""

import base64
import json
import secrets
import subprocess
import sys
from urllib.parse import quote


def run(args):
    return subprocess.run(args, check=True, text=True, capture_output=True).stdout


try:
    secret_arn = run(["terraform", "-chdir=deploy/terraform", "output", "-raw", "database_secret_arn"]).strip()
    db_host = run(["terraform", "-chdir=deploy/terraform", "output", "-raw", "database_endpoint"]).strip()
    db_secret = json.loads(run(["aws", "secretsmanager", "get-secret-value", "--secret-id", secret_arn, "--output", "json"]))
    credentials = json.loads(db_secret["SecretString"])
    old = {}
    try:
        old = json.loads(run(["kubectl", "-n", "edgevault", "get", "secret", "edgevault-runtime", "-o", "json"]))
        old = {k: base64.b64decode(v).decode() for k, v in old.get("data", {}).items()}
    except subprocess.CalledProcessError:
        pass
except (KeyError, ValueError, subprocess.CalledProcessError) as exc:
    print(f"runtime secret: {exc}", file=sys.stderr)
    sys.exit(2)

dsn = "postgres://{}:{}@{}:{}/edgevault?sslmode=verify-full".format(
    quote(credentials["username"], safe=""), quote(credentials["password"], safe=""),
    db_host, credentials.get("port", 5432),
)
data = {
    "database-url": dsn,
    "chunk-hmac-key": old.get("chunk-hmac-key") or secrets.token_hex(32),
    "admin-token": old.get("admin-token") or secrets.token_urlsafe(48),
}
manifest = {
    "apiVersion": "v1",
    "kind": "Secret",
    "metadata": {"name": "edgevault-runtime", "namespace": "edgevault"},
    "type": "Opaque",
    "data": {k: base64.b64encode(v.encode()).decode() for k, v in data.items()},
}
json.dump(manifest, sys.stdout)
sys.stdout.write("\n")
