# Maven repo without Last-Modified. Artifact "headfail" answers HEAD with
# 503; "slowhead" hangs 300s on HEAD.
import http.server, sys, os
LOG = open(sys.argv[2], "a", buffering=1)
POM = b'<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>%s</artifactId><version>%s</version></project>'
META = b'<metadata><groupId>com.example</groupId><artifactId>%s</artifactId><versioning><latest>1.1</latest><release>1.1</release><versions><version>1.0</version><version>1.1</version></versions><lastUpdated>20261113000000</lastUpdated></versioning></metadata>'
class H(http.server.BaseHTTPRequestHandler):
    def body(self):
        p = self.path.strip("/").split("/")
        if len(p) == 4 and p[3] == "maven-metadata.xml": return META % p[2].encode()
        if len(p) == 5 and p[4] == f"{p[2]}-{p[3]}.pom": return POM % (p[2].encode(), p[3].encode())
        if len(p) == 5 and p[4] == f"{p[2]}-{p[3]}.jar": return b"PK\x05\x06" + b"\0"*18
        return None
    def reply(self, send):
        LOG.write(f"{self.command} {self.path}\n")
        b = self.body()
        if self.command == "HEAD" and "/slowhead/" in self.path:
            import time; time.sleep(300)
        if self.command == "HEAD" and "/headfail/" in self.path:
            self.send_response(503); self.send_header("Content-Length","0"); self.end_headers(); return
        if b is None:
            self.send_response(404); self.send_header("Content-Length","0"); self.end_headers(); return
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers()
        if send: self.wfile.write(b)
    def do_GET(self): self.reply(True)
    def do_HEAD(self): self.reply(False)
    def log_message(self, *a): pass
    def date_time_string(self, *a): return super().date_time_string(*a)
http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
