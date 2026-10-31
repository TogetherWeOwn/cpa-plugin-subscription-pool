"""Keep deploy script tests off live CLIProxy.

verify-live.sh, verify-live-opencodego.sh and rollback.sh talk to the
management API. Their tests replace curl with a PATH stub, but a bypassed stub
used to send fixture keys to the live port 127.0.0.1:8317, and five of those
ban every caller on the host for 30 minutes. One way the stub gets bypassed:
a BASH_ENV file that resets PATH in every non-interactive bash.

Importing this module, in every test module that runs one of those scripts:

- starts a tripwire HTTP server on an ephemeral 127.0.0.1 port and exports it
  as CLIPROXY_TEST_MANAGEMENT_ORIGIN and CLIPROXY_MANAGEMENT_URL, so the
  scripts approve only the tripwire and refuse the live port;
- drops BASH_ENV and ENV so the curl stub stays first on PATH;
- provides tearDownModule, which fails the module if any request reached the
  tripwire (a stub was bypassed).

Tests build their environment from os.environ.copy(), so every script run
inherits these settings.
"""

import http.server
import os
import threading

HITS = []


class _Tripwire(http.server.BaseHTTPRequestHandler):
    def _record(self):
        HITS.append(f"{self.command} {self.path}")
        self.send_response(503)
        self.end_headers()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = _record

    def log_message(self, *_args):
        pass


_SERVER = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Tripwire)
threading.Thread(target=_SERVER.serve_forever, daemon=True).start()
ORIGIN = f"http://127.0.0.1:{_SERVER.server_port}"

os.environ["CLIPROXY_TEST_MANAGEMENT_ORIGIN"] = ORIGIN
os.environ["CLIPROXY_MANAGEMENT_URL"] = ORIGIN
os.environ.pop("BASH_ENV", None)
os.environ.pop("ENV", None)


def tearDownModule():
    if HITS:
        raise AssertionError(f"a curl stub was bypassed; requests reached the test origin: {HITS}")
