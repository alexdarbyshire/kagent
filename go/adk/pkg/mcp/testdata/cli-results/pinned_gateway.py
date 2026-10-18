#!/usr/bin/env python3
"""Characterize pinned gateway output through the candidate MCP executable.

Run via TestCLIPinnedGatewayProjection with the two documented binary paths.
Every HTTP response is synthetic and served on loopback by this fixture.
"""

import argparse
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading


GATEWAY_SHA256 = "da103875206c49ebb0f0b732ff201a33c4e11c55126fdb6b5abf970e89e2908c"
PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1ZkAAAAASUVORK5CYII="
)
PDF = b"%PDF-1.7\n%\xff\xfe\x80\nsynthetic binary fixture\n%%EOF\n"
JSON_BODY = json.dumps({"output": {
    "base64_string": base64.b64encode(PDF).decode("ascii"),
    "sha256": hashlib.sha256(PDF).hexdigest(),
}, "page_mapping": [2, 1]}).encode()
RESPONSES = {
    "/json": ("application/json", JSON_BODY),
    "/png": ("image/png", PNG),
    "/pdf": ("application/pdf", PDF),
}


def inspect(directory, cli):
    for kind, target, conversion, expected in [
        ("json", "output.base64_string", "base64", PDF),
        ("png", "content.image", "base64", PNG),
        ("pdf", "message", "text", PDF.decode("utf-8", errors="replace").encode()),
    ]:
        args = [str(cli), "--binding-file", str(directory / "binding.json"),
                kind + "_result", "--save-" + conversion, target + "=" + kind,
                "--output-file=" + kind + ".json"]
        if kind == "json":
            args.extend(["--verify-sha256", "output.sha256"])
        result = subprocess.run(args, cwd=directory, capture_output=True, timeout=30)
        assert result.returncode == 0, (kind, result.returncode, result.stderr.decode(),
                                       (directory / (kind + ".json")).read_text())
        assert result.stdout == b"", result.stdout
        saved = (directory / kind).read_bytes()
        assert saved == expected, (kind, saved, expected)
        envelope = json.loads((directory / (kind + ".json")).read_bytes())
        if kind == "json":
            assert envelope["structuredContent"] == json.loads(JSON_BODY)
            assert envelope["content"][0]["type"] == "text"
        elif kind == "png":
            assert envelope["content"][0]["type"] == "image"
        else:
            assert envelope["content"][0]["type"] == "text"
            assert envelope["structuredContent"] == {"code": 200, "message": expected.decode()}
            assert saved != PDF
            assert b"\xef\xbf\xbd" in saved
        print(json.dumps({"case": kind, "bytes": len(saved),
                          "sha256": hashlib.sha256(saved).hexdigest(),
                          "reversible_binary": kind != "pdf"}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gateway", type=Path)
    parser.add_argument("--cli", type=Path, required=True)
    parser.add_argument("--inspect", type=Path)
    args = parser.parse_args()
    if args.inspect:
        inspect(args.inspect, args.cli)
        return
    assert hashlib.sha256(args.gateway.read_bytes()).hexdigest() == GATEWAY_SHA256
    calls = []

    class Origin(BaseHTTPRequestHandler):
        def do_GET(self):
            calls.append(self.path)
            content_type, body = RESPONSES[self.path]
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass

    origin = ThreadingHTTPServer(("127.0.0.1", 0), Origin)
    thread = threading.Thread(target=origin.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="mcp-cli-gateway-") as temporary:
            directory = Path(temporary)
            schema = {"openapi": "3.0.3", "info": {"title": "Synthetic output", "version": "1"},
                      "paths": {"/" + kind: {"get": {"operationId": kind + "_result",
                                "responses": {"200": {"description": "Synthetic response"}}}}
                                for kind in ("json", "png", "pdf")}}
            (directory / "openapi.json").write_text(json.dumps(schema))
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            config = {"config": {"adminAddr": "127.0.0.1:0"},
                      "gateways": {"fixture": {"port": port, "bindAddress": "127.0.0.1"}},
                      "routes": [{"gateways": "fixture", "backends": [{"mcp": {"targets": [{
                          "name": "fixture", "openapi": {
                              "schema": {"file": str(directory / "openapi.json")},
                              "host": "127.0.0.1:" + str(origin.server_port)}}]}}]}]}
            (directory / "gateway.json").write_text(json.dumps(config))
            (directory / "binding.json").write_text(json.dumps({"name": "fixture", "http": {
                "params": {"url": "http://127.0.0.1:" + str(port) + "/mcp"}}}))
            result = subprocess.run([
                str(args.gateway), "oneshot", "-f", str(directory / "gateway.json"),
                "--output", str(directory / "gateway.log"), sys.executable,
                str(Path(__file__).resolve()), "--cli", str(args.cli), "--inspect", str(directory),
            ], capture_output=True, timeout=120)
            if result.returncode:
                print((directory / "gateway.log").read_text(), file=sys.stderr)
            assert result.returncode == 0, (result.stdout.decode(), result.stderr.decode())
            print(result.stdout.decode(), end="")
            assert calls == ["/json", "/png", "/pdf"], calls
    finally:
        origin.shutdown()
        origin.server_close()
        thread.join()


if __name__ == "__main__":
    main()
