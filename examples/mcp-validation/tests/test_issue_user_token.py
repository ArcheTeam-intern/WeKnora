"""Credential issuer contracts; HTTP responses are fixtures, never live passwords."""
import base64
import importlib.util
import io
import json
from pathlib import Path
import urllib.error

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "issue_user_token.py"


def test_standalone_utility_exists_outside_agent():
    assert SCRIPT.exists(), "standalone official-login credential utility is missing"


@pytest.fixture
def issuer():
    if not SCRIPT.exists():
        pytest.skip("utility has not been implemented")
    spec = importlib.util.spec_from_file_location("issue_user_token", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def jwt(exp=2000000000):
    # Fixture represents a server-issued token; the utility never creates JWTs.
    payload = base64.urlsafe_b64encode(json.dumps({"exp": exp}).encode()).decode().rstrip("=")
    return "fixture-header." + payload + ".fixture-server-signature"


@pytest.fixture
def http(monkeypatch, issuer):
    calls, responses = [], []
    def urlopen(request, timeout):
        calls.append(request)
        status, body = responses.pop(0)
        raw = json.dumps(body).encode()
        if status != 200:
            raise urllib.error.HTTPError(request.full_url, status, "error", {}, io.BytesIO(raw))
        class Response:
            def __enter__(self): return self
            def __exit__(self, *args): return False
            def read(self): return raw
        return Response()
    monkeypatch.setattr(issuer.urllib.request, "urlopen", urlopen)
    return calls, responses


def response(**changes):
    value = {"success": True, "token": jwt(), "user": {"id": "u1", "email": "15918868657@163.com", "username": "Member", "tenant_id": 99},
             "active_tenant": {"id": 2}, "memberships": [{"tenant_id": 2, "role": "viewer"}]}
    value.update(changes)
    return value


def test_official_login_writes_profile_and_server_jwt(issuer, http):
    calls, responses = http
    responses.append((200, response()))
    result = issuer.issue_token("http://localhost:18082/", issuer.DEFAULT_EMAIL, "fixture-password")
    assert calls[0].full_url == "http://localhost:18082/api/v1/auth/login"
    assert json.loads(calls[0].data) == {"email": issuer.DEFAULT_EMAIL, "password": "fixture-password"}
    assert result == {"profile": {"email": issuer.DEFAULT_EMAIL, "name": "Member", "user_id": "u1", "tenant_id": "2"},
                      "token": jwt(), "expires_at": "2033-05-18T03:33:20Z"}


@pytest.mark.parametrize("status,payload", [(401, {"message": "fixture-password"}), (200, response(token="")),
    (200, response(token="not-a-fixture-jwt")), (200, response(token=jwt(1))), (200, response(success=False)),
    (200, response(token=jwt(None)))])
def test_bad_credentials_or_invalid_jwt_fail_without_secrets(issuer, http, status, payload):
    http[1].append((status, payload))
    with pytest.raises(issuer.CredentialError) as error:
        issuer.issue_token("http://localhost:18082", issuer.DEFAULT_EMAIL, "fixture-password")
    assert "fixture-password" not in str(error.value)
    assert payload.get("token", "not-secret") not in str(error.value) if payload.get("token") else True


def test_multiple_workspaces_require_explicit_selection(issuer, http):
    http[1].append((200, response(memberships=[{"tenant_id": 2}, {"tenant_id": 3}])))
    with pytest.raises(issuer.CredentialError, match="workspace-id"):
        issuer.issue_token("http://localhost:18082", issuer.DEFAULT_EMAIL, "fixture-password")
    assert len(http[0]) == 1


def test_explicit_workspace_switch_uses_official_membership_checked_api(issuer, http):
    http[1].extend([(200, response(memberships=[{"tenant_id": 2}, {"tenant_id": 3}])),
                   (200, response(active_tenant={"id": 3}))])
    result = issuer.issue_token("http://localhost:18082", issuer.DEFAULT_EMAIL, "fixture-password", workspace_id="3")
    request = http[0][1]
    assert request.full_url.endswith("/api/v1/auth/switch-tenant")
    assert json.loads(request.data) == {"tenant_id": 3}
    assert request.get_header("Authorization") == "Bearer " + jwt()
    assert result["profile"]["tenant_id"] == "3"


def test_nonmember_workspace_is_rejected_without_switch(issuer, http):
    http[1].append((200, response()))
    with pytest.raises(issuer.CredentialError):
        issuer.issue_token("http://localhost:18082", issuer.DEFAULT_EMAIL, "fixture-password", workspace_id="8")
    assert len(http[0]) == 1


def test_cli_defaults_prompts_password_and_prints_only_metadata(issuer, http, tmp_path, monkeypatch, capsys):
    http[1].append((200, response()))
    monkeypatch.setattr(issuer.getpass, "getpass", lambda prompt: "fixture-password")
    output = tmp_path / "token.local.json"
    assert issuer.main(["--output", str(output)]) == 0
    saved = json.loads(output.read_text(encoding="utf-8"))
    assert saved["token"] == jwt()
    printed = capsys.readouterr()
    assert "fixture-password" not in printed.out + printed.err
    assert jwt() not in printed.out + printed.err
    assert json.loads(printed.out)["output"] == str(output)


def test_explicit_accounts_file_matches_requested_email_only(issuer, http, tmp_path, capsys):
    http[1].append((200, response()))
    accounts = tmp_path / "accounts.json"
    accounts.write_text(json.dumps({"accounts": [{"email": "other@example.com", "password": "wrong"},
        {"email": issuer.DEFAULT_EMAIL, "password": "fixture-password"}]}))
    assert issuer.main(["--accounts-file", str(accounts), "--output", str(tmp_path / "token.json")]) == 0
    assert json.loads(http[0][0].data)["password"] == "fixture-password"
    assert "fixture-password" not in str(capsys.readouterr())


def test_help_has_no_password_command_line_argument(issuer, capsys):
    with pytest.raises(SystemExit) as error:
        issuer.main(["--help"])
    assert error.value.code == 0
    help_text = capsys.readouterr().out
    assert "--password" not in help_text
    assert "--workspace-id" in help_text
    assert "--accounts-file" in help_text


def test_cli_authentication_failure_never_prints_upstream_body(issuer, http, monkeypatch, capsys):
    http[1].append((401, {"message": "fixture-password " + jwt()}))
    monkeypatch.setattr(issuer.getpass, "getpass", lambda prompt: "fixture-password")
    assert issuer.main([]) == 1
    text = str(capsys.readouterr())
    assert "fixture-password" not in text and jwt() not in text
    assert "401" in text


def test_cli_rejects_password_argument_without_echoing_its_value(issuer, capsys):
    with pytest.raises(SystemExit) as error:
        issuer.main(["--password", "fixture-password"])
    assert error.value.code == 2
    assert "fixture-password" not in str(capsys.readouterr())


@pytest.mark.parametrize("working_directory", ["repository", "other"])
def test_default_output_stays_in_ignored_deploy_directory_regardless_cwd(
        issuer, http, tmp_path, monkeypatch, capsys, working_directory):
    import subprocess
    repository = SCRIPT.parents[3]
    expected = SCRIPT.parents[1] / "deploy" / "workbench-user-token.local.json"
    monkeypatch.chdir(repository if working_directory == "repository" else tmp_path)
    http[1].append((200, response()))
    monkeypatch.setattr(issuer.getpass, "getpass", lambda prompt: "fixture-password")
    saved_paths = []
    def save(path, credentials):
        saved_paths.append(Path(path).resolve())
        return Path(path).resolve()
    monkeypatch.setattr(issuer, "_save_credentials", save)
    assert issuer.main([]) == 0
    assert saved_paths == [expected]
    ignored = subprocess.run(["git", "check-ignore", "--", str(expected.relative_to(repository))],
        cwd=repository, capture_output=True, text=True)
    assert ignored.returncode == 0
    assert "fixture-password" not in str(capsys.readouterr())
