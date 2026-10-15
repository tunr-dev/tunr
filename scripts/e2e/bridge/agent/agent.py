"""e2e hosted agent: POST /run (signed by the relay) → count invoices → /data."""
import hashlib, hmac, http.server, os, subprocess, time

SECRET = os.environ["TUNR_TRIGGER_SECRET"].encode()
TUNR = "/opt/tunr/bin/tunr"


def verified(header: str, body: bytes) -> bool:
    try:
        parts = dict(p.split("=", 1) for p in header.split(","))
        ts = parts["t"]
    except (KeyError, ValueError):
        return False
    mac = hmac.new(SECRET, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts.get("v1", "")) and abs(time.time() - int(ts)) < 300


def count_invoices() -> str:
    # Identity comes from TUNR_AGENT_BOOTSTRAP; tunr exchanges it for a token.
    p = subprocess.Popen([TUNR, "access", "acme/erp-db", "--port", "15432"],
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    dsn = None
    deadline = time.time() + 30
    while time.time() < deadline:
        line = p.stdout.readline()
        if not line:
            break
        if line.startswith("postgresql://"):
            dsn = line.strip()
            break
    if not dsn:
        p.kill()
        return "no-dsn:" + p.stderr.read()[-300:].replace("\n", " ")
    out = subprocess.run(["psql", dsn, "-tAc", "select count(*) from fatura"], capture_output=True, text=True)
    p.terminate()
    return out.stdout.strip() or ("error:" + out.stderr.strip())


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"agent ok")

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if not verified(self.headers.get("X-Tunr-Trigger-Signature", ""), body):
            self.send_response(401)
            self.end_headers()
            return
        result = count_invoices()
        with open("/data/runs.log", "a") as f:
            f.write(f"{self.headers.get('X-Tunr-Trigger')} {result}\n")
        self.send_response(200)
        self.end_headers()
        self.wfile.write(result.encode())


http.server.ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", "8080"))), Handler).serve_forever()
