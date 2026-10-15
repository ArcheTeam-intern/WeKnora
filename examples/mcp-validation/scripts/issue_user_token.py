#!/usr/bin/env python3
"""Obtain a genuine WeKnora user JWT through its official authentication API.

Run outside the Agent runtime, then import the resulting credential JSON into
its workbench. Decoding exp is informational; only WeKnora authenticates JWTs.
"""
import argparse
import base64
from datetime import datetime, timezone
import getpass
import json
import os
import sys
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_EMAIL = "15918868657@163.com"


class CredentialError(ValueError):
    """A safe user-facing error; never include upstream bodies or exceptions."""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Passwords and bearer tokens must stay on the explicitly chosen origin.
        return None


def _post(base_url, path, payload, token=None):
    request = urllib.request.Request(base_url.rstrip("/") + path,
        data=json.dumps(payload).encode("utf-8"), method="POST",
        headers={"Content-Type": "application/json", "Accept": "application/json"})
    if token:
        request.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            result = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        if exc.code in (400, 401):
            raise CredentialError("Official authentication rejected the credentials (HTTP 401).") from None
        raise CredentialError(f"Official authentication failed (HTTP {exc.code}).") from None
    except Exception:
        raise CredentialError("Cannot read the official authentication response; check the service URL and connectivity.") from None
    if not isinstance(result, dict) or result.get("success") is False:
        raise CredentialError("Official authentication did not succeed.")
    return result


def _expiry(token):
    try:
        parts = token.split(".")
        if len(parts) != 3 or not all(parts):
            raise ValueError
        claims = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
        exp = claims.get("exp")
        if isinstance(exp, bool) or not isinstance(exp, (int, float)) or exp <= time.time():
            raise ValueError
        return datetime.fromtimestamp(exp, timezone.utc).isoformat().replace("+00:00", "Z")
    except Exception:
        raise CredentialError("Official login returned a missing, malformed, or expired JWT expiry.") from None


def _issued_token(response):
    token = response.get("token")
    if not isinstance(token, str) or not token.strip():
        raise CredentialError("Official login returned no user JWT.")
    _expiry(token)
    return token


def issue_token(base_url, email, password, workspace_id=None):
    parsed = urllib.parse.urlsplit(base_url)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise CredentialError("Use an http(s) service base URL without credentials, query, or fragment.")
    result = _post(base_url, "/api/v1/auth/login", {"email": email, "password": password})
    token = _issued_token(result)
    memberships = result.get("memberships") or []
    member_ids = {str(item.get("tenant_id")) for item in memberships if isinstance(item, dict) and item.get("tenant_id")}
    active_id = str((result.get("active_tenant") or {}).get("id") or "")
    if not active_id:
        raise CredentialError("Official login did not identify an active workspace.")
    if len(member_ids) > 1 and workspace_id is None:
        raise CredentialError("Multiple workspaces are available; pass --workspace-id explicitly.")
    if workspace_id is not None:
        selected = str(workspace_id)
        if not selected.isdigit() or int(selected) <= 0 or selected not in member_ids:
            raise CredentialError("Requested workspace is absent from the user's official memberships.")
        if selected != active_id:
            result = _post(base_url, "/api/v1/auth/switch-tenant", {"tenant_id": int(selected)}, token)
            token = _issued_token(result)
            active_id = str((result.get("active_tenant") or {}).get("id") or "")
            if active_id != selected:
                raise CredentialError("Official workspace switch did not return the selected workspace.")
    user = result.get("user") or {}
    if not isinstance(user, dict) or not user.get("id"):
        raise CredentialError("Official login did not return a user profile.")
    return {"profile": {"email": user.get("email") or email,
            "name": user.get("username") or user.get("name") or email,
            "user_id": str(user["id"]), "tenant_id": active_id},
            "token": token, "expires_at": _expiry(token)}


def _password_from_file(path, email):
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8-sig"))
        entries = raw.get("accounts") if isinstance(raw, dict) else raw
        matches = [item for item in entries if isinstance(item, dict)
                   and str(item.get("email") or "").strip().lower() == email.lower()]
        if len(matches) != 1 or not isinstance(matches[0].get("password"), str) or not matches[0]["password"]:
            raise ValueError
        return matches[0]["password"]
    except Exception:
        raise CredentialError("Cannot load exactly one matching email with a password from the explicit accounts file.") from None


def _save_credentials(path, credentials):
    # Restrictive creation mode on POSIX; no source password is written.
    path = Path(path).expanduser().resolve()
    temporary = path.with_name(path.name + ".tmp")
    created = False
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        created = True
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            json.dump(credentials, output, ensure_ascii=False, indent=2)
            output.write("\n")
        temporary.replace(path)
    except OSError:
        if created and temporary.exists():
            temporary.unlink()
        raise CredentialError("Cannot save credential JSON; check the output directory and permissions.") from None
    return path


class _SafeArgumentParser(argparse.ArgumentParser):
    def error(self, message):
        # An accidentally supplied password flag must not echo its value.
        self.print_usage(sys.stderr)
        self.exit(2, "Invalid arguments; use --help. Passwords are entered at the secure prompt.\n")


def main(argv=None):
    parser = _SafeArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://localhost:18082", help="Official WeKnora service base URL")
    parser.add_argument("--email", default=DEFAULT_EMAIL)
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().parents[1] / "deploy" / "workbench-user-token.local.json")
    parser.add_argument("--workspace-id", help="Explicit official membership workspace ID")
    parser.add_argument("--accounts-file", type=Path, help="Explicit local accounts JSON for a one-time test; otherwise prompt securely")
    args = parser.parse_args(argv)
    try:
        email = args.email.strip()
        password = _password_from_file(args.accounts_file, email) if args.accounts_file else getpass.getpass("WeKnora password: ")
        if not email or not password:
            raise CredentialError("Email and password are required.")
        credentials = issue_token(args.base_url, email, password, args.workspace_id)
        output = _save_credentials(args.output, credentials)
    except CredentialError as exc:
        print(f"Credential issuance failed: {exc}", file=sys.stderr)
        return 1
    except (EOFError, KeyboardInterrupt):
        print("Credential issuance cancelled.", file=sys.stderr)
        return 1
    print(json.dumps({"output": str(output), "profile": credentials["profile"],
                      "expires_at": credentials["expires_at"]}, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    # Disable redirects so a service misconfiguration cannot forward credentials.
    urllib.request.install_opener(urllib.request.build_opener(_NoRedirect()))
    raise SystemExit(main())
