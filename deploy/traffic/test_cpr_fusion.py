"""Isolated real application + PostgreSQL/Redis + synthetic CPR transport.

Requires FULLAPP_BIN and FULLAPP_FIXTURE_BIN built from this checkout.
All containers and the internal network are owned by a random fixture namespace.
No production endpoints, users, keys, accounts or balances are read or modified.
"""
from concurrent.futures import ThreadPoolExecutor
import ipaddress
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.error
import urllib.request


def main():
    binary = Path(os.environ["FULLAPP_BIN"]).resolve(strict=True)
    fixture = Path(os.environ["FULLAPP_FIXTURE_BIN"]).resolve(strict=True)
    docker = os.environ.get("DOCKER_BIN", "docker")
    prefix = "s2-bootstrap-cpr-" + secrets.token_hex(5)
    network = prefix + "-net"
    app, pg, redis, model = [prefix + "-" + kind for kind in ("app", "pg", "redis", "model")]
    password, key = secrets.token_urlsafe(30), "sk-fixture-" + secrets.token_hex(20)
    owned = []
    hosts = []
    network_created = False
    stage = "preflight"
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def cmd(args, payload=None, timeout=60):
        proc = subprocess.run([docker] + args, input=payload, capture_output=True, timeout=timeout)
        if proc.returncode:
            raise RuntimeError("fixture command failed at " + stage)
        return proc.stdout

    def create(name, args):
        owned.append(name)
        cmd(["run", "-d", "--name", name, "--network", network] + hosts + args, timeout=180)

    def internal_endpoint(name, port):
        data = json.loads(cmd(["inspect", name]))[0]
        if not data["State"]["Running"]:
            raise RuntimeError("fixture process exited at " + stage)
        networks = data["NetworkSettings"]["Networks"]
        assert set(networks) == {network}, "fixture network isolation lost"
        assert not data["HostConfig"].get("PortBindings"), "fixture must not publish host ports"
        address = ipaddress.ip_address(networks[network]["IPAddress"])
        assert address.version == 4 and address.is_private and not address.is_loopback, "invalid fixture address"
        return str(address) + ":" + str(port)

    def request(endpoint, path, body=None, auth=False, token=None, method=None):
        headers = {}
        if auth:
            headers["Authorization"] = "Bearer " + key
        if token:
            headers["Authorization"] = "Bearer " + token
        raw = None
        if body is not None:
            raw = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request("http://" + endpoint + path, data=raw, headers=headers, method=method)
        with opener.open(req, timeout=30) as response:
            return response.status, response.read()

    def wait(check, timeout=60, container=None):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if container and not json.loads(cmd(["inspect", container]))[0]["State"]["Running"]:
                raise RuntimeError("fixture process exited at " + stage)
            try:
                value = check()
                if value:
                    return value
            except Exception:
                pass
            time.sleep(0.3)
        raise RuntimeError("fixture wait timed out at " + stage)

    try:
        cmd(["network", "create", "--internal", network])
        network_created = True
        stage = "database"
        create(pg, ["--memory", "1g", "--cpus", "1", "--tmpfs", "/var/lib/postgresql",
                    "-e", "POSTGRES_USER=fixture", "-e", "POSTGRES_DB=bootstrap_fixture",
                    "-e", "POSTGRES_PASSWORD=" + password, "postgres:18-bookworm"])
        hosts.extend(["--add-host", pg + ":" + internal_endpoint(pg, 5432).split(":")[0]])
        create(redis, ["--memory", "256m", "--cpus", "0.5", "--tmpfs", "/data", "redis:8-alpine",
                       "redis-server", "--save", "", "--appendonly", "no"])
        hosts.extend(["--add-host", redis + ":" + internal_endpoint(redis, 6379).split(":")[0]])
        wait(lambda: subprocess.run([docker, "exec", pg, "pg_isready", "-h", "127.0.0.1", "-U", "fixture",
                                     "-d", "bootstrap_fixture"], capture_output=True).returncode == 0)
        stage = "synthetic-model"
        create(model, ["--memory", "256m", "--cpus", "1",
                       "-v", str(fixture) + ":/fixture:ro", "-e", "CI_FULLAPP_FIXTURE=1",
                       "golang:1.27.3-alpine", "/fixture", "serve"])
        model_port = internal_endpoint(model, 9090)
        hosts.extend(["--add-host", model + ":" + model_port.split(":")[0]])
        wait(lambda: request(model_port, "/fixture/stats")[0] == 200)
        stage = "application"
        env = {
            "AUTO_SETUP": "true", "SERVER_HOST": "0.0.0.0", "SERVER_PORT": "8080",
            "SERVER_MODE": "release", "DATABASE_HOST": pg, "DATABASE_PORT": "5432",
            "DATABASE_USER": "fixture", "DATABASE_PASSWORD": password,
            "DATABASE_DBNAME": "bootstrap_fixture", "DATABASE_SSLMODE": "disable",
            "REDIS_HOST": redis, "REDIS_PORT": "6379", "REDIS_DB": "0",
            "ADMIN_EMAIL": "admin-fixture@example.invalid", "ADMIN_PASSWORD": secrets.token_urlsafe(24),
            "JWT_SECRET": secrets.token_hex(32), "TOTP_ENCRYPTION_KEY": secrets.token_hex(32),
            "SECURITY_URL_ALLOWLIST_ENABLED": "false", "SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP": "true",
            "SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS": "true", "IP_RISK_DISABLED": "true",
            "GOMAXPROCS": "2", "GOMEMLIMIT": "1500MiB", "TZ": "UTC",
        }
        args = ["--memory", "2g", "--cpus", "2",
                "--tmpfs", "/app/data", "-w", "/app", "-v", str(binary) + ":/app/sub2api:ro"]
        for name, value in env.items():
            args.extend(["-e", name + "=" + value])
        create(app, args + ["golang:1.27.3-alpine", "/app/sub2api"])
        port = internal_endpoint(app, 8080)
        hosts.extend(["--add-host", app + ":" + port.split(":")[0]])
        wait(lambda: request(port, "/health")[0] == 200, 180, app)
        origin = "http://" + model + ":9090"
        data = {
            "database": {"host": pg, "port": 5432, "user": "fixture", "password": password,
                         "dbname": "bootstrap_fixture", "sslmode": "disable"},
            "redis": {"host": redis, "port": 6379, "db": 0},
            "origin": origin, "api_key": key,
        }

        def operate(operation):
            return json.loads(cmd(["run", "--rm", "-i", "--network", network] + hosts + ["--memory", "512m",
                                   "-v", str(fixture) + ":/fixture:ro", "-e", "CI_FULLAPP_FIXTURE=1",
                                   "golang:1.27.3-alpine", "/fixture", operation],
                                  json.dumps(data).encode()))

        stage = "synthetic-admin-ui-state"
        operate("seed-admin-ui")
        stage = "global-gateway-and-client-key-admin-api"
        login = json.loads(request(port, "/api/v1/auth/login", {
            "email": env["ADMIN_EMAIL"], "password": env["ADMIN_PASSWORD"],
        })[1])
        admin_token = login["data"]["access_token"]
        settings_path = "/api/v1/admin/accounts/cpr/gateway"
        for method, body in (("GET", None), ("PUT", {"base_url": "https://untrusted.invalid"})):
            try:
                request(port, settings_path, body, method=method)
            except urllib.error.HTTPError as error:
                assert error.code == 401, "global gateway must require admin authentication"
                error.close()
            else:
                raise RuntimeError("unauthenticated global gateway access")
        initial = json.loads(request(port, settings_path, token=admin_token)[1])["data"]
        assert initial["base_url"] == "", "unexpected pre-existing CPR configuration"
        saved = json.loads(request(port, settings_path, {"base_url": origin},
                                  token=admin_token, method="PUT")[1])["data"]
        assert saved["base_url"] == origin, "global gateway not saved"
        try:
            request(port, settings_path, {}, token=admin_token, method="PUT")
        except urllib.error.HTTPError as error:
            assert error.code == 400, "missing base_url must not clear the shared destination"
            error.close()
        else:
            raise RuntimeError("missing gateway field was accepted")
        channel = json.loads(request(port, "/api/v1/admin/accounts", {
            "name": "existing-pool-key-api-fixture", "platform": "openai", "type": "cpr",
            "credentials": {"api_key": "synthetic-upstream-admin-only"},
            "concurrency": 2, "priority": 50, "group_ids": [],
        }, token=admin_token)[1])["data"]
        assert channel["type"] == "cpr", "CPR client key was not accepted"
        assert not any(field in channel["credentials"] for field in
                       ("api_key", "admin_api_key", "cpr_account_id", "base_url")), "credential response leaked legacy binding or key"
        usage = json.loads(request(port, "/api/v1/admin/accounts/" + str(channel["id"]) + "/usage",
                                   token=admin_token)[1])["data"]
        assert usage["cpr"] == {"mode": "client_key", "quota_source": "cpr", "quota_available": False}, "single-account quota still queried"
        assert json.loads(request(model_port, "/fixture/stats")[1])["accepted"] == 0, "configuration must not generate inference"
        stage = "seed"
        seeded = operate("seed-cpr")
        data.update({field: seeded[field] for field in ("user_id", "api_key_id", "account_id")})
        models = json.loads(request(port, "/v1/models", auth=True)[1])
        assert models == {"object": "list", "data": [{"id": "gpt-6.1-sol", "object": "model", "owned_by": "fixture-pool"}]}, "CPR key model scope was not relayed"
        stage = "relay-concurrency-and-billing"

        def inference():
            status, body = request(port, "/v1/responses", {
                "model": "gpt-6.1-sol", "stream": True,
                "input": "fixture-hold-until-release", "instructions": "Synthetic fixture only.",
            }, True)
            if status != 200 or b"response.completed" not in body or b"response.failed" in body:
                raise RuntimeError("synthetic relay did not complete")
            return body

        with ThreadPoolExecutor(max_workers=3) as pool:
            first, second = pool.submit(inference), pool.submit(inference)
            wait(lambda: json.loads(request(model_port, "/fixture/stats")[1])["accepted"] == 2)
            third = pool.submit(inference)
            time.sleep(0.5)
            assert json.loads(request(model_port, "/fixture/stats")[1])["accepted"] == 2, "concurrency exceeded"
            request(model_port, "/fixture/release", {})
            for task in (first, second, third):
                task.result(timeout=20)
        status = wait(lambda: (lambda value: value if value["usage_rows"] == 3 and value["dedup_rows"] == 3 else None)(operate("status")), 30)
        assert status["input_tokens"] == 21 and status["output_tokens"] == 6, "usage mismatch"
        assert 0 < status["balance"] < 10, "billing was not applied"
        assert all(status[name] == 0 for name in ("user_slots", "account_slots", "api_key_slots", "api_key_queue_active", "api_key_queue_waiting")), "slot leak"
        stage = "websocket-turns-and-billing"
        assert operate("probe-cpr-ws")["ws_turns"] == 2, "WebSocket rounds did not complete"
        status = wait(lambda: (lambda value: value if value["usage_rows"] == 5 and value["dedup_rows"] == 5 else None)(operate("status")), 30)
        assert status["input_tokens"] == 35 and status["output_tokens"] == 10, "WebSocket usage mismatch"
        assert all(status[name] == 0 for name in ("user_slots", "account_slots", "api_key_slots", "api_key_queue_active", "api_key_queue_waiting")), "WebSocket slot leak"
        assert json.loads(request(model_port, "/fixture/stats")[1])["admin_requests"] == 0, "CPR admin API must never be called"
        return {"passed": True, "real_application": True, "synthetic_upstream_only": True,
                "global_gateway_admin_api": True, "existing_client_key_only": True,
                "model_manifest_relayed": True, "cpr_admin_requests": 0,
                "sse_completed": 3, "websocket_completed": 2,
                "usage_rows": status["usage_rows"], "dedup_rows": status["dedup_rows"],
                "input_tokens": status["input_tokens"], "output_tokens": status["output_tokens"],
                "balance": status["balance"], "concurrency_limit_verified": 2, "slots_released": True}
    finally:
        cleanup_failed = False
        for name in reversed(owned):
            removed = subprocess.run([docker, "rm", "-f", "--volumes", name], capture_output=True, timeout=45)
            cleanup_failed = cleanup_failed or removed.returncode != 0
        if network_created:
            removed = subprocess.run([docker, "network", "rm", network], capture_output=True, timeout=30)
            cleanup_failed = cleanup_failed or removed.returncode != 0
        if cleanup_failed:
            raise RuntimeError("fixture cleanup incomplete; inspect the generated fixture namespace")


if __name__ == "__main__":
    try:
        result = main()
    except Exception as exc:
        print(json.dumps({"passed": False, "error": str(exc)[:200]}))
        raise SystemExit(1)
    print(json.dumps(result))
