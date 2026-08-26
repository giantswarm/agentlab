#!/usr/bin/env python3
"""
Real OIDC authorization-code flow against the lab Dex: opens the browser on
Dex's own login screen, catches the callback on 127.0.0.1:5555, exchanges the
code for tokens and writes kubeconfig.oidc.

This is the flow to show a client -- they see the Dex login page and type
credentials for a user that exists nowhere but this cluster.
"""
import base64, http.server, json, os, secrets, ssl, subprocess, sys, threading
import urllib.parse, urllib.request, webbrowser

# Unbuffered, so the auth URL shows up immediately even when piped.
sys.stdout.reconfigure(line_buffering=True)

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ISSUER = "https://127.0.0.1:32000/dex"
CLIENT_ID = "kubernetes"
CLIENT_SECRET = "kubernetes-lab-secret"
REDIRECT = "http://127.0.0.1:5555/callback"
CA = os.path.join(ROOT, "certs", "ca.crt")

ctx = ssl.create_default_context(cafile=CA)
state = secrets.token_urlsafe(16)
result = {}
done = threading.Event()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        if "code" not in q:
            self.send_response(400); self.end_headers()
            self.wfile.write(b"no code in callback")
            return
        if q.get("state", [None])[0] != state:
            self.send_response(400); self.end_headers()
            self.wfile.write(b"state mismatch")
            return
        result["code"] = q["code"][0]
        done.set()
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(
            b"<html><body style='font-family:sans-serif;padding:3rem'>"
            b"<h2>Logged in.</h2><p>You can close this tab and go back to the terminal.</p>"
            b"</body></html>")

    def log_message(self, *a):
        pass


def main():
    auth_url = ISSUER + "/auth?" + urllib.parse.urlencode({
        "client_id": CLIENT_ID,
        "redirect_uri": REDIRECT,
        "response_type": "code",
        "scope": "openid email profile groups offline_access",
        "state": state,
    })

    srv = http.server.HTTPServer(("127.0.0.1", 5555), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    print("Opening the Dex login page in your browser...")
    print("  (users: admin@lab.local / dev@lab.local / viewer@lab.local, password: password)")
    print(f"  if it does not open: {auth_url}\n")
    webbrowser.open(auth_url)

    done.wait(timeout=300)
    srv.shutdown()
    if "code" not in result:
        sys.exit("timed out waiting for the browser callback")

    data = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "code": result["code"],
        "redirect_uri": REDIRECT,
    }).encode()
    basic = base64.b64encode(f"{CLIENT_ID}:{CLIENT_SECRET}".encode()).decode()
    req = urllib.request.Request(ISSUER + "/token", data=data,
                                 headers={"Authorization": "Basic " + basic})
    tok = json.loads(urllib.request.urlopen(req, context=ctx).read())

    id_token = tok["id_token"]
    payload = id_token.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))

    print("id_token claims:")
    print(json.dumps(claims, indent=2))

    open(os.path.join(ROOT, ".token"), "w").write(id_token)
    subprocess.run([os.path.join(ROOT, "scripts", "mk-kubeconfig.sh"),
                    id_token, os.path.join(ROOT, "kubeconfig.oidc")],
                   check=True, stdout=subprocess.DEVNULL, cwd=ROOT)

    print(f"\nLogged in as {claims.get('email')}.")
    print(f"  export KUBECONFIG={os.path.join(ROOT, 'kubeconfig.oidc')}")
    print( "  kubectl auth whoami")


if __name__ == "__main__":
    sys.exit(main())
