#!/usr/bin/env python3
"""Receiver used by tests/e2e.sh.

  srv.py HTTP_PORT UDP_PORT LOG_DIR [SMTP_PORT]

HTTP (127.0.0.1:HTTP_PORT) appends one JSON line per POST to LOG_DIR/http.log
(path, Authorization header, body). Paths starting with /slow wait 5 seconds
before answering; paths starting with /err answer 500; anything else answers 200.

UDP (127.0.0.1:UDP_PORT) appends every datagram, one per line, to
LOG_DIR/syslog.log. The same port number is also listened on over TCP; each
received line is appended to LOG_DIR/syslog-tcp.log.

SMTP (127.0.0.1:SMTP_PORT, only if given) accepts any message without
encryption or login and writes each one to LOG_DIR/mail-N.eml (CRLF as sent).
"""
import http.server
import json
import os
import socket
import sys
import threading
import time

http_port, udp_port, log_dir = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
smtp_port = int(sys.argv[4]) if len(sys.argv) > 4 else 0
os.makedirs(log_dir, exist_ok=True)
lock = threading.Lock()


METRICS = b"""# TYPE node_cpu_info gauge
node_cpu_info{core="0",package="0"} 1
node_cpu_info{core="1",package="0"} 1
node_cpu_info{core="0",package="1"} 1
"""


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        # like an exporter: /metrics works, /metrics/ does not
        ok = self.path.split("?")[0] == "/metrics"
        self.send_response(200 if ok else 404)
        self.end_headers()
        self.wfile.write(METRICS if ok else b"not found")

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
        with lock, open(os.path.join(log_dir, "http.log"), "a") as f:
            f.write(json.dumps({"path": self.path,
                                "auth": self.headers.get("Authorization"),
                                "body": body}) + "\n")
        if self.path.startswith("/slow"):
            time.sleep(5)
        try:
            self.send_response(500 if self.path.startswith("/err") else 200)
            self.end_headers()
            self.wfile.write(b"{}")
        except (BrokenPipeError, ConnectionResetError):
            pass  # the client gave up (its timeout fired), which is the point of /slow

    def log_message(self, *args):
        pass


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True


threading.Thread(target=Server(("127.0.0.1", http_port), Handler).serve_forever, daemon=True).start()

def tcp_syslog(conn):
    with conn, conn.makefile("rb") as f:
        for line in f:
            with lock, open(os.path.join(log_dir, "syslog-tcp.log"), "ab") as out:
                out.write(line.rstrip(b"\r\n") + b"\n")


def smtp(conn):
    n = [0]
    with conn, conn.makefile("rwb") as f:
        def say(t):
            f.write(t.encode() + b"\r\n")
            f.flush()
        say("220 e2e ESMTP")
        for raw in f:
            cmd = raw.decode(errors="replace").strip()
            up = cmd.upper()
            if up == "DATA":
                say("354 go")
                body = []
                for l in f:
                    if l == b".\r\n":
                        break
                    body.append(l)
                with lock:
                    n[0] = len([x for x in os.listdir(log_dir) if x.startswith("mail-")]) + 1
                    with open(os.path.join(log_dir, "mail-%d.eml" % n[0]), "wb") as out:
                        out.write(b"".join(body))
                say("250 queued")
            elif up == "QUIT":
                say("221 bye")
                return
            else:
                say("250 ok")


def listen(port, handler):
    ls = socket.socket()
    ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    ls.bind(("127.0.0.1", port))
    ls.listen(8)
    while True:
        c, _ = ls.accept()
        threading.Thread(target=handler, args=(c,), daemon=True).start()


threading.Thread(target=listen, args=(udp_port, tcp_syslog), daemon=True).start()
if smtp_port:
    threading.Thread(target=listen, args=(smtp_port, smtp), daemon=True).start()

udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
udp.bind(("127.0.0.1", udp_port))
while True:
    data, _ = udp.recvfrom(65535)
    with lock, open(os.path.join(log_dir, "syslog.log"), "ab") as f:
        f.write(data + b"\n")
