#!/usr/bin/env python3
"""Smoke HTTP contra um banco NOVO, já migrado; não use um banco de produção."""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


def main():
    """Copia pastas, aprova consentimento, executa o guest, mostra headers e desativa."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True, help="Binário backend compilado")
    parser.add_argument("--database-url", required=True, help="Banco descartável já migrado")
    parser.add_argument("--port", type=int, default=18085)
    args = parser.parse_args()
    server = str(Path(args.server).resolve())
    base = f"http://127.0.0.1:{args.port}"
    cookie = ""
    process = None
    with tempfile.TemporaryDirectory(prefix="papo-plugins-smoke-") as work:
        plugins = Path(work) / "plugins"
        plugins.mkdir()
        env = os.environ.copy()
        env.update(
            SERVER_PORT=str(args.port), DATABASE_URL=args.database_url,
            JWT_SECRET="local-plugins-smoke-test-secret-32-characters",
            HMAC_SECRET="local-plugins-smoke-media-secret", BASE_URL=base,
            PLUGINS_DIR=str(plugins), PLUGINS_HOT_RELOAD="false",
            MODERATION_ENABLED="false", USE_FCM_RELAY="false",
            VOICE_ICE_UDP_PORT="0", AUTH_RATE_LIMIT="100", AUTH_RATE_BURST="100",
            RATE_LIMIT="100", RATE_BURST="100",
        )
        log = open(Path(work) / "server.log", "w+")

        def request(method, path, body=None):
            """Mantém o cookie só na API; a rota UI é testada sem credenciais via curl."""
            data = json.dumps(body).encode() if body is not None else None
            headers = {"Content-Type": "application/json"}
            if cookie:
                headers["Cookie"] = cookie
            req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
            try:
                with urllib.request.urlopen(req, timeout=10) as response:
                    payload = response.read()
                    result = json.loads(payload) if response.headers.get_content_type() == "application/json" else payload.decode()
                    return response.status, result, response.headers
            except urllib.error.HTTPError as error:
                raise AssertionError(f"{method} {path}: {error.code} {error.read().decode()}") from error

        def start():
            """Espera health sem depender de um atraso fixo de inicialização."""
            nonlocal process
            process = subprocess.Popen([server], cwd=work, env=env, stdout=log, stderr=log)
            for _ in range(100):
                try:
                    request("GET", "/health")
                    return
                except Exception:
                    if process.poll() is not None:
                        log.seek(0)
                        raise RuntimeError(log.read())
                    time.sleep(0.05)
            raise RuntimeError("servidor não ficou pronto")

        def stop():
            """Usa shutdown normal para verificar que o runtime não deixa o processo preso."""
            nonlocal process
            if process and process.poll() is None:
                process.send_signal(signal.SIGINT)
                process.wait(timeout=15)
            process = None

        try:
            start()
            for plugin in ("bluemap", "reference"):
                shutil.copytree(Path(__file__).resolve().parent / plugin, plugins / plugin)
            _, discovered, _ = request("POST", "/auth/register", {"username": "smoke" + uuid.uuid4().hex[:8], "password": "SmokeLocal!12345"})
            username = discovered["username"]
            _, _, headers = request("POST", "/auth/login", {"username": username, "password": "SmokeLocal!12345"})
            cookie = headers["Set-Cookie"].split(";", 1)[0]
            request("POST", "/server", {"name": "Plugins smoke", "public": True})
            _, channel, _ = request("POST", "/channels", {"name": "plugin-messages", "type": "text"})
            _, catalog, _ = request("POST", "/plugins/rescan")
            assert {p["id"] for p in catalog["plugins"]} == {"bluemap", "reference"}
            print("PASS: copy folders → rescan discovers both plugins")
            installed = {}
            for plugin in ("bluemap", "reference"):
                settings = {"map_url": "https://8.8.8.8/map"} if plugin == "bluemap" else {"target_channel": channel["id"]}
                selection = {"settings": settings}
                _, preview, _ = request("POST", f"/plugins/{plugin}/preview", selection)
                if plugin == "reference":
                    assert preview["scopes"] == [channel["id"]]
                selection.update(consent=preview["consent"], expires_at=preview["expires_at"])
                _, installed[plugin], _ = request("POST", f"/plugins/{plugin}/enable", selection)
                assert installed[plugin]["kind"] == "plugin" and installed[plugin]["state"] == "enabled"
            _, channels, _ = request("GET", "/channels")
            apps = [c for c in channels["channels"] if c["type"] == "app"]
            assert len(apps) == 2
            print("PASS: preview → enable both → two app channels exist")
            _, output, _ = request("POST", "/plugins/reference/trigger", {})
            assert output["ok"]
            message = output["result"]
            assert "asset.read" in message["content"]
            assert message["app_installation_id"] == installed["reference"]["id"]
            assert len(message["embeds"]) == 1
            embed = message["embeds"][0]
            assert embed["source_type"] == "app" and embed["app"]["surface_id"] == "result" and embed["app"]["available"]
            print("PASS: reference trigger reads template and posts own inline embed")
            ui = base + "/apps/" + installed["reference"]["id"] + "/ui/index.html"
            headers = subprocess.check_output(["curl", "-fsS", "-D", "-", "-o", str(Path(work) / "ui.html"), ui], text=True)
            assert "sandbox allow-scripts allow-forms" in headers and "allow-same-origin" not in headers
            assert "nosniff" in headers and "no-referrer" in headers and "Set-Cookie:" not in headers
            print("PASS: curl UI without cookie; response headers follow:")
            print(headers.strip())
            request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.put", "key": "counter", "value": 42}})
            stop()
            start()
            _, stored, _ = request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.get", "key": "counter"}})
            assert stored["ok"] and stored["result"]["value"] == 42
            print("PASS: full backend restart retains app_storage and installation metadata")
            for plugin in ("reference", "bluemap"):
                assert request("POST", f"/plugins/{plugin}/disable")[0] == 204
            _, channels, _ = request("GET", "/channels")
            assert not any(c["type"] == "app" for c in channels["channels"])
            status = subprocess.check_output(["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", ui], text=True)
            assert status == "404"
            print("PASS: disable both → app channels gone → UI returns 404")
        finally:
            stop()
            log.close()


if __name__ == "__main__":
    main()
