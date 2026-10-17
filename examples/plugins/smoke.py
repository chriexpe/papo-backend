#!/usr/bin/env python3
"""Smoke HTTP contra um banco NOVO, já migrado; não use um banco de produção."""
import argparse
import base64
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
    parser.add_argument("--avatar-browser", help="Verificador WPE nativo, opcional; recebe a URL somente por stdin")
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
            HMAC_SECRET="local-plugins-smoke-media-secret", BASE_URL=base, APPS_ORIGIN="",
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
            owner_id = discovered["id"]
            image = base64.b64encode((Path(__file__).resolve().parent / "reference/icon.png").read_bytes()).decode()
            request("PUT", f"/users/{owner_id}/avatar", {"avatar": image, "avatar_format": "PNG"})
            request("PUT", f"/users/{owner_id}", {"nickname": "Jogador Um", "status": "online", "description": ""})
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
            _, configured, _ = request("PATCH", "/plugins/reference", {"storage_quota": {"max_keys": 512, "max_total_bytes": 2097152, "max_value_bytes": 32768}})
            assert configured["storage_quota"] == {"max_keys": 512, "max_total_bytes": 2097152, "max_value_bytes": 32768}
            print("PASS: admin PATCH applies per-plugin storage quotas")
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
            assert embed["source_type"] == "app" and embed["surface"]["id"] == "result" and embed["entry"]
            print("PASS: reference trigger reads template and posts own inline embed")
            ui = base + "/apps/" + installed["reference"]["id"] + "/ui/" + installed["reference"]["plugin_digest"] + "/index.html"
            headers = subprocess.check_output(["curl", "-fsS", "-D", "-", "-o", str(Path(work) / "ui.html"), ui], text=True)
            assert "sandbox allow-scripts allow-forms" in headers and "allow-same-origin" not in headers
            assert "nosniff" in headers and "no-referrer" in headers and "Set-Cookie:" not in headers
            assert "Cross-Origin-Resource-Policy: cross-origin" in headers
            assert "Access-Control-Allow-Origin: *" in headers and "Access-Control-Allow-Credentials:" not in headers
            assert "default-src 'self' " + base in headers
            print("PASS: curl UI without cookie; response headers follow:")
            print(headers.strip())
            # Todos os recursos reais são públicos para o sandbox opaco, inclusive em 304.
            ui_root = ui.rsplit("/", 1)[0] + "/"
            assets = {"index.html": "text/html", "result.html": "text/html",
                      "style.css": "text/css", "main.mjs": "javascript",
                      "image.svg": "image/svg+xml", "fixture.ttf": "font/ttf",
                      "data.json": "application/json"}
            for name, mime in assets.items():
                req = urllib.request.Request(ui_root + name, headers={"Origin": "null"})
                with urllib.request.urlopen(req) as response:
                    payload = response.read()
                    assert payload and mime in response.headers["Content-Type"]
                    assert response.headers["Access-Control-Allow-Origin"] == "*"
                    assert response.headers.get("Access-Control-Allow-Credentials") is None
                    assert response.headers.get("Set-Cookie") is None
                    etag = response.headers["ETag"]
                    if name.endswith(".html"):
                        assert all(asset.encode() in payload for asset in ("main.mjs", "style.css", "image.svg"))
                    if name == "data.json":
                        assert json.loads(payload)["message"]
                cached = urllib.request.Request(ui_root + name, headers={"Origin": "null", "If-None-Match": etag})
                try:
                    urllib.request.urlopen(cached)
                    raise AssertionError("revalidação deveria retornar 304")
                except urllib.error.HTTPError as response:
                    assert response.code == 304
                    assert response.headers["Access-Control-Allow-Origin"] == "*"
                    assert response.headers.get("Access-Control-Allow-Credentials") is None
                    assert response.headers["Cross-Origin-Resource-Policy"] == "cross-origin"
            print("PASS: both pages and module/image/font/fetch JSON assets with Origin:null; 200 and 304")
            # Duas sessões humanas distintas percorrem convite e uma partida autoritativa.
            owner_cookie = cookie
            player_name = "player" + uuid.uuid4().hex[:8]
            _, player, _ = request("POST", "/auth/register", {"username": player_name, "password": "SmokeLocal!12345"})
            _, _, login_headers = request("POST", "/auth/login", {"username": player_name, "password": "SmokeLocal!12345"})
            player_cookie = login_headers["Set-Cookie"].split(";", 1)[0]
            cookie = player_cookie
            request("PUT", f"/users/{player['id']}/avatar", {"avatar": image, "avatar_format": "PNG"})
            request("PUT", f"/users/{player['id']}", {"nickname": "Jogadora Dois", "status": "online", "description": ""})
            cookie = owner_cookie
            app_id = installed["reference"]["id"]
            app_channel = next(c for c in apps if c["app"]["app_installation_id"] == app_id)
            assert app_channel["app"]["user_actions"][0]["id"] == "convidar"
            status, _, _ = request("POST", "/interactions", {
                "app_installation_id": app_id, "channel_id": app_channel["id"],
                "kind": "user_action", "action_id": "convidar", "target_user_id": player["id"],
            })
            assert status == 202
            cookie = player_cookie
            _, notifications, _ = request("GET", f"/users/{player['id']}/notifications")
            invite = next(n for n in notifications["notifications"] if n["kind"] == "app_invite")
            assert invite["message_id"] is None
            _, accepted, _ = request("POST", f"/notifications/{invite['id']}/accept")
            assert accepted["channel_id"] == app_channel["id"]

            def bridge(path, body, token=None):
                headers = {"Content-Type": "application/json", "Origin": "null"}
                if token:
                    headers["Authorization"] = "Bearer " + token
                req = urllib.request.Request(base + f"/apps/{app_id}/" + path, data=json.dumps(body).encode(), headers=headers, method="POST")
                with urllib.request.urlopen(req) as response:
                    assert response.headers["Access-Control-Allow-Origin"] == "*"
                    assert response.headers.get("Access-Control-Allow-Credentials") is None
                    return json.load(response)

            def open_session(invite_id=None):
                selection = {"surface_id": "home", "channel_id": app_channel["id"]}
                if invite_id:
                    selection["invite_id"] = invite_id
                _, ticket, _ = request("POST", f"/apps/{app_id}/launch", selection)
                return bridge("session", {"ticket": ticket["ticket"]})["token"]

            player_token = open_session(invite["id"])
            cookie = owner_cookie
            owner_token = open_session()
            def rpc(token, method, params=None):
                result = bridge("rpc", {"method": method, "params": params or {}}, token)
                assert result["ok"], result
                return result["result"]
            assert rpc(player_token, "identity")["user_id"] == player["id"]
            profiles = rpc(owner_token, "game.profiles")
            assert {p["display_name"] for p in profiles} == {"Jogador Um", "Jogadora Dois"}
            assert all(set(p) == {"id", "username", "display_name", "avatar_url"} for p in profiles)
            for profile in profiles:
                assert profile["avatar_url"] == f"/users/{profile['id']}/avatar"
                req = urllib.request.Request(base + profile["avatar_url"], headers={"Origin": "null"})
                with urllib.request.urlopen(req) as response:
                    assert response.headers["Cross-Origin-Resource-Policy"] == "cross-origin"
                    assert response.headers["Access-Control-Allow-Origin"] == "*"
                    assert response.headers.get("Access-Control-Allow-Credentials") is None
                    assert response.read().startswith(b"\x89PNG")
            print("PASS: users.resolve returns four public fields; both avatars load by credentialless HTTP with CORP/CORS")
            assert rpc(owner_token, "game.state")["game"]["revision"] == 0
            started = time.perf_counter()
            moved = rpc(owner_token, "game.move", {"cell": 0, "revision": 0})
            acknowledged_ms = (time.perf_counter() - started) * 1000
            assert moved["game"]["board"][0] == "X"
            # Observador na próxima rodada do polling padrão, sem SSE ou socket do jogo.
            time.sleep(max(0, 1.5 - (time.perf_counter() - started)))
            seen = rpc(player_token, "game.state")
            observed_ms = (time.perf_counter() - started) * 1000
            assert seen["game"]["board"][0] == "X" and seen["game"]["revision"] == 1
            assert rpc(player_token, "game.move", {"cell": 4, "revision": 1})["game"]["board"][4] == "O"
            for token, cell, revision in [(owner_token, 1, 2), (player_token, 3, 3), (owner_token, 2, 4)]:
                final = rpc(token, "game.move", {"cell": cell, "revision": revision})
            assert final["game"]["winner"] == "X"
            print(f"PASS: two players invite → accept → launch → verified RPC → full game; move ack={acknowledged_ms:.1f} ms, second player observes={observed_ms:.1f} ms with 1500 ms polling (local HTTP, not browser rendering)")
            _, chat, _ = request("GET", f"/channels/{channel['id']}/messages")
            game_message = next(m for m in chat["messages"] if m["id"] == final["game"]["message_id"])
            assert game_message["components"][0][1]["opens"] == "result"
            assert game_message["embeds"][0]["display"] == "overlay"
            assert request("POST", "/interactions", {"app_installation_id": app_id, "channel_id": channel["id"], "kind": "button", "message_id": game_message["id"], "component_id": "revanche"})[0] == 202
            assert rpc(player_token, "game.state")["game"]["winner"] == ""
            print("PASS: message components and opens overlay; Revanche resets persisted game")
            _, only, _ = request("POST", "/plugins/reference/trigger", {"request": {
                "method": "message.send", "channel_id": channel["id"],
                "components": [[{"id": "abrir", "label": "Jogar", "style": "primary", "opens": "result", "disabled": False}]],
            }})
            assert only["ok"] and only["result"]["content"] is None
            only_id = only["result"]["id"]
            _, cleared, _ = request("POST", "/plugins/reference/trigger", {"request": {
                "method": "message.edit", "channel_id": channel["id"], "message_id": only_id, "components": [],
            }})
            assert cleared["ok"] and not cleared["result"].get("components")
            print("PASS: buttons-only message and explicit component removal")
            if args.avatar_browser:
                for surface, file in (("home", "index.html"), ("result", "result.html")):
                    selection = {"surface_id": surface, "channel_id": app_channel["id"] if surface == "home" else channel["id"]}
                    if surface == "result":
                        selection["message_id"] = game_message["id"]
                    _, ticket, _ = request("POST", f"/apps/{app_id}/launch", selection)
                    page = base + ticket["surface"]["entry"].rsplit("/", 1)[0] + f"/{file}#papo_ticket=" + ticket["ticket"]
                    checked = subprocess.run([str(Path(args.avatar_browser).resolve())], input=page + "\n", text=True, capture_output=True, timeout=30)
                    if checked.returncode:
                        # O ticket não é exibido, nem em falha do verificador.
                        raise AssertionError(f"WPE não confirmou avatares em {file}: código {checked.returncode}; " + checked.stderr.replace(ticket["ticket"], "[ticket]"))
                    assert "PASS: WPE sandbox" in checked.stdout
                    print(f"PASS: WPE real, contexto efêmero e CSP sandbox: nomes e dois avatares decodificados em {file}")
            _, large, _ = request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.put", "key": "large-quota", "value": "x" * 20000}})
            assert large["ok"]
            request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.put", "key": "counter", "value": 42}})
            stop()
            start()
            _, stored, _ = request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.get", "key": "counter"}})
            assert stored["ok"] and stored["result"]["value"] == 42
            _, catalog, _ = request("GET", "/plugins")
            reference = next(p for p in catalog["plugins"] if p["id"] == "reference")
            assert reference["installation"]["storage_quota"]["max_value_bytes"] == 32768
            _, large, _ = request("POST", "/plugins/reference/trigger", {"request": {"method": "kv.get", "key": "large-quota"}})
            assert large["ok"] and len(large["result"]["value"]) == 20000
            print("PASS: 20 KiB host value and admin quota override persist across backend restart")
            print("PASS: full backend restart retains app_storage and installation metadata")
            for plugin in ("reference", "bluemap"):
                assert request("POST", f"/plugins/{plugin}/disable")[0] == 204
            _, channels, _ = request("GET", "/channels")
            assert not any(c["type"] == "app" for c in channels["channels"])
            status = subprocess.check_output(["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", ui], text=True)
            assert status == "404"
            try:
                rpc(owner_token, "identity")
                raise AssertionError("sessão sobreviveu disable")
            except urllib.error.HTTPError as response:
                assert response.code == 401
            print("PASS: disabled plugin revokes UI sessions")
            print("PASS: disable both → app channels gone → UI returns 404")
        finally:
            stop()
            log.close()


if __name__ == "__main__":
    main()
