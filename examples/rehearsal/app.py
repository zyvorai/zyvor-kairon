#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""Offline SQLite + real HTTP compatibility fixture, using only Python stdlib.
Install at /opt/kairon-rehearsal/app.py on a Linux guest with python3 and QGA.
The probe starts a temporary loopback HTTP server and exercises its /health API.
"""
import argparse
import http.server
import pathlib
import sqlite3
import threading
import urllib.error
import urllib.request


def probe(url, expect_schema_failure=False):
    try:
        with urllib.request.urlopen(url, timeout=3) as response:
            return 2 if expect_schema_failure else (0 if response.status == 200 else 1)
    except urllib.error.HTTPError as error:
        if expect_schema_failure:
            return 1 if error.code == 503 and error.read(128) == b"schema incompatible" else 2
        return 1
    except urllib.error.URLError:
        return 2 if expect_schema_failure else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["init", "probe", "serve", "compatible", "breaking", "repair"])
    parser.add_argument("--db", default="/var/lib/kairon-rehearsal/app.db")
    parser.add_argument("--url")
    parser.add_argument("--expect-schema-failure", action="store_true")
    parser.add_argument("--bind", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--port-file")
    args = parser.parse_args()
    if args.action == "probe" and args.url:
        return probe(args.url, args.expect_schema_failure)
    with sqlite3.connect(args.db) as db:
        if args.action == "init":
            db.execute("CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name TEXT)")
            db.execute("INSERT OR IGNORE INTO users VALUES (1, 'Kairon')")
            return 0
        if args.action == "compatible":
            db.execute("ALTER TABLE users ADD COLUMN note TEXT")
            return 0
        if args.action == "repair":
            db.execute("ALTER TABLE customers RENAME TO users")
            return 0
        if args.action == "breaking":
            db.execute("ALTER TABLE users RENAME TO customers")
            return 0

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            try:
                with sqlite3.connect(args.db) as db:
                    row = db.execute("SELECT name FROM users WHERE id=1").fetchone()
                healthy = row == ("Kairon",)
            except sqlite3.Error:
                healthy = False
            self.send_response(200 if healthy else 503)
            self.end_headers()
            self.wfile.write(b"healthy" if healthy else b"schema incompatible")

        def log_message(self, *_args):
            pass

    server = http.server.HTTPServer((args.bind, args.port), Handler)
    if args.action == "serve":
        if args.port_file:
            port_path = pathlib.Path(args.port_file)
            temporary = port_path.with_name(port_path.name + ".tmp")
            temporary.write_text(str(server.server_port))
            temporary.replace(port_path)
        try:
            server.serve_forever()
        finally:
            server.server_close()
        return 0
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        return probe(f"http://127.0.0.1:{server.server_port}/health", args.expect_schema_failure)
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    raise SystemExit(main())
