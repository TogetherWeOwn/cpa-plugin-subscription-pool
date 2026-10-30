#!/usr/bin/env python3
"""Real curl against loopback: bounded token reception, HTTP errors and redirects.

The curl wrapper validates the original request before rewriting only its URL.
It forwards bytes without imposing a size limit itself. Receiver-side forwarded
bytes and the measured pipe buffer detect late, post-download validation;
server socket writes/BrokenPipe are diagnostics only, not receiver consumption.
No registry traffic or fixture-host execution is permitted.
"""
from dataclasses import dataclass
import gzip
import hashlib
import http.client
import http.server
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.parse

REPOSITORY = "example/fixture-host"
TOKEN_CEILING = 1024 * 1024
OVERSIZED_LENGTH = 2 * TOKEN_CEILING
CHUNK_SIZE = 64 * 1024
HOST_BYTES = b"#!/bin/sh\nprintf 'must-not-execute\\n' >\"$HOST_FIXTURE_EXECUTED_MARKER\"\n"
EXPECTED_TOKEN_JSON = b'{"token": "loopback-token-sentinel"}'


@dataclass(frozen=True)
class CaseSpec:
    # All oracle inputs are mandatory and independent of the generated fixture.
    # Omitting them must fail closed, never adopt fixture constructor defaults.
    name: str
    mode: str
    phase: str
    two_layers: bool
    token_length: int
    receiver_delay: float
    want_success: bool


def digest(body):
    return "sha256:" + hashlib.sha256(body).hexdigest()


def layer_archive(name, body, mode=0o644):
    archive = io.BytesIO()
    with tarfile.open(fileobj=archive, mode="w") as tar:
        member = tarfile.TarInfo(name)
        member.size, member.mode = len(body), mode
        tar.addfile(member, io.BytesIO(body))
    return gzip.compress(archive.getvalue(), mtime=0)


class RegistryFixture:
    def __init__(self, directory, mode="normal", phase="token", two_layers=True,
                 token_length=OVERSIZED_LENGTH, receiver_delay=0):
        self.directory, self.mode, self.phase = Path(directory), mode, phase
        self.token = "loopback-token-sentinel"
        self.token_body = json.dumps({"token": self.token}).encode()
        if token_length < len(self.token_body):
            raise ValueError("fixture length cannot contain the complete token JSON")
        self.stream_body = b" " * (token_length - len(self.token_body)) + self.token_body
        self.receiver_delay = receiver_delay
        self.host_path = "CLIProxyAPI/CLIProxyAPI"
        self.layers = [layer_archive(self.host_path, HOST_BYTES, 0o755)]
        if two_layers:
            self.layers.append(layer_archive("EXTRA/second.txt", b"distinct second-layer bytes\n"))
        self.blobs = {digest(body): body for body in self.layers}
        self.manifest = json.dumps({"schemaVersion": 2, "layers": [
            {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
             "digest": digest(body), "size": len(body)} for body in self.layers
        ]}).encode()
        self.manifest_digest = digest(self.manifest)
        self.pin = self.directory / "pin.json"
        self.pin.write_text(json.dumps({"repository": REPOSITORY,
                                      "manifest_digest": self.manifest_digest,
                                      "platform": "linux/amd64"}))
        self.out = self.directory / "out"
        self.marker = self.directory / "host-executed"
        self.requests, self.transfers = [], []
        self.lock = threading.Lock()

    def __enter__(self):
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def setup(self):
                super().setup()
                self.connection.settimeout(3)

            def log_message(self, *_args):
                pass

            def flush_headers(self):
                # Observe the serialized headers actually written to the socket,
                # independently of the mode label and framing branch below.
                raw = b"".join(self._headers_buffer)
                super().flush_headers()
                headers = http.client.parse_headers(io.BytesIO(raw.split(b"\r\n", 1)[1]))
                self.emitted_headers = {name.lower(): headers.get_all(name) for name in headers}

            def do_GET(self):
                self.emitted_headers = {}
                parsed = urllib.parse.urlsplit(self.path)
                redirect = parsed.path.startswith("/final/")
                path = parsed.path.removeprefix("/final") if redirect else parsed.path
                if path == "/token":
                    phase, identity, body = "token", REPOSITORY, fixture.token_body
                    valid = urllib.parse.parse_qs(parsed.query) == {
                        "service": ["registry.docker.io"],
                        "scope": [f"repository:{REPOSITORY}:pull"],
                    }
                elif path == f"/v2/{REPOSITORY}/manifests/{fixture.manifest_digest}":
                    phase, identity, body, valid = "manifest", fixture.manifest_digest, fixture.manifest, True
                elif path.startswith(f"/v2/{REPOSITORY}/blobs/"):
                    phase, identity = "blob", path.rsplit("/", 1)[-1]
                    body, valid = fixture.blobs.get(identity, b""), identity in fixture.blobs
                else:
                    self.send_error(404)
                    return
                authorized = phase == "token" or self.headers.get("Authorization") == f"Bearer {fixture.token}"
                with fixture.lock:
                    fixture.requests.append((phase, identity, redirect, valid, authorized))
                    attempt = sum(row[0] == phase and not row[2] for row in fixture.requests)
                code = 200
                targeted = phase == fixture.phase and (phase != "blob" or identity == digest(fixture.layers[0]))
                if not redirect and targeted and fixture.mode == "401":
                    code = 401
                elif not redirect and targeted and fixture.mode == "redirect":
                    code = 302
                framed_token = phase == "token" and fixture.mode in ("declared", "chunked")
                chunked = framed_token and fixture.mode == "chunked"
                if framed_token:
                    body = fixture.stream_body
                partial = phase == "token" and fixture.mode == "partial" and attempt == 1
                intended, sent, broken = len(body), 0, False
                complete_body_digest = digest(body)
                observed_body_digest = hashlib.sha256()
                self.close_connection = True
                try:
                    self.send_response(code)
                    self.send_header("Connection", "close")
                    if chunked:
                        self.send_header("Transfer-Encoding", "chunked")
                    else:
                        self.send_header("Content-Length", str(intended))
                    if code == 302:
                        self.send_header("Location", "/final" + self.path)
                    self.end_headers()
                    if partial:
                        body = body[:len(body) // 2]
                    for offset in range(0, len(body), CHUNK_SIZE):
                        block = body[offset:offset + CHUNK_SIZE]
                        if chunked:
                            self.wfile.write(f"{len(block):x}\r\n".encode())
                        self.wfile.write(block)
                        if chunked:
                            self.wfile.write(b"\r\n")
                        self.wfile.flush()
                        sent += len(block)
                        observed_body_digest.update(block)
                        if framed_token:
                            # Allow a receiver to close; no elapsed-time assertion.
                            time.sleep(0.005)
                    if chunked:
                        self.wfile.write(b"0\r\n\r\n")
                        self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                    broken = True
                finally:
                    if phase == "token":
                        with fixture.lock:
                            fixture.transfers.append({"intended": intended,
                                                      "body_digest": complete_body_digest,
                                                      "sent": sent, "broken": broken,
                                                      "observed_body_digest": "sha256:" + observed_body_digest.hexdigest(),
                                                      "headers": self.emitted_headers,
                                                      "status": code, "partial": partial})

        class Server(http.server.ThreadingHTTPServer):
            daemon_threads = False
            block_on_close = True

        self.server = Server(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02})
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.server.server_close()  # Joins every non-daemon request thread.
        self.thread.join()


def option_value(arguments, name):
    for index, argument in enumerate(arguments):
        if argument == name and index + 1 < len(arguments):
            return arguments[index + 1]
        if argument.startswith(name + "="):
            return argument.split("=", 1)[1]
    return None


def curl_body_sink(arguments):
    outputs = []
    for index, argument in enumerate(arguments):
        if argument in ("--output", "-o"):
            outputs.append(arguments[index + 1] if index + 1 < len(arguments) else None)
        elif argument.startswith("--output="):
            outputs.append(argument.split("=", 1)[1])
        elif argument.startswith("-o") and len(argument) > 2:
            outputs.append(argument[2:])
    if any(output != "-" for output in outputs) or any(
            argument in ("--remote-name", "--remote-name-all", "-O") for argument in arguments):
        return "output-file"
    if any(argument in ("--config", "-K") or argument.startswith(("--config=", "-K"))
           for argument in arguments):
        return "unobserved"
    if any(argument in ("--write-out", "-w") or argument.startswith(("--write-out=", "-w"))
           for argument in arguments):
        return "mixed-stdout"
    return "stdout"


def curl_wrapper(arguments):
    """Validate identity, then execute real curl with flags/body semantics intact."""
    config = json.loads(Path(os.environ["HTTP_FIXTURE_CONFIG"]).read_text())
    log = Path(config["log"])
    urls = [argument for argument in arguments if argument.startswith(("http://", "https://"))]
    identity = config["identities"].get(urls[0]) if len(urls) == 1 else None
    if identity is None:
        with log.open("a") as output:
            output.write(json.dumps({"contract_error": "unexpected request identity"}) + "\n")
        return 64
    phase, key = identity
    local_path = "/token?service=registry.docker.io&scope=repository:example/fixture-host:pull" if phase == "token" else urllib.parse.urlsplit(urls[0]).path
    rewritten = f"http://127.0.0.1:{config['port']}{local_path}"
    body_sink = curl_body_sink(arguments)
    forwarded = stdout_bytes = 0
    pipe_capacity = 0
    try:
        import fcntl
        pipe_capacity = fcntl.fcntl(1, fcntl.F_GETPIPE_SZ)
    except (OSError, AttributeError):
        pass  # An --output file need not use a stdout pipe.
    command = [config["curl"], "-q", "--noproxy", "*", "--proxy", ""]
    command += [rewritten if argument == urls[0] else argument for argument in arguments]
    child = subprocess.Popen(command, stdout=subprocess.PIPE, stdin=subprocess.DEVNULL)
    broken = False
    receiver_delayed = False
    applied_receiver_delay = observed_receiver_delay = 0
    try:
        while True:
            block = os.read(child.stdout.fileno(), 4096)
            if not block:
                break
            while block:
                try:
                    if phase == "token" and body_sink == "stdout" and config["receiver_delay"] and not receiver_delayed:
                        # Deterministic descheduling at the first forwarded write.
                        # The sender may finish into TCP buffers while we pause.
                        applied_receiver_delay = config["receiver_delay"]
                        pause_started = time.monotonic()
                        time.sleep(applied_receiver_delay)
                        observed_receiver_delay = time.monotonic() - pause_started
                        receiver_delayed = True
                    count = os.write(1, block)
                except BrokenPipeError:
                    broken = True
                    child.kill()
                    break
                stdout_bytes += count
                if body_sink == "stdout":
                    forwarded += count
                block = block[count:]
            if broken:
                break
    finally:
        child.stdout.close()
        status = child.wait()
        record = {"phase": phase, "identity": key, "forwarded": forwarded,
                  "body_sink": body_sink, "stdout_bytes": stdout_bytes,
                  "pipe_capacity": pipe_capacity, "broken": broken,
                  "receiver_delayed": receiver_delayed,
                  "receiver_delay_seconds": applied_receiver_delay,
                  "observed_receiver_delay_seconds": observed_receiver_delay,
                  "fail": any(flag in arguments for flag in ("--fail", "--fail-with-body", "-f")),
                  "location": any(flag in arguments for flag in ("--location", "-L")),
                  "max_filesize": option_value(arguments, "--max-filesize")}
        with log.open("a") as output:
            output.write(json.dumps(record) + "\n")
    return 23 if status < 0 else status


def run_extractor(fixture, extractor, real_curl):
    wrapper_dir = fixture.directory / "bin"
    wrapper_dir.mkdir()
    (wrapper_dir / "curl").symlink_to(Path(__file__).resolve())
    (wrapper_dir / "sleep").symlink_to(shutil.which("true"))
    identities = {f"https://auth.docker.io/token?service=registry.docker.io&scope=repository:{REPOSITORY}:pull": ["token", REPOSITORY],
                  f"https://registry-1.docker.io/v2/{REPOSITORY}/manifests/{fixture.manifest_digest}": ["manifest", fixture.manifest_digest]}
    identities.update({f"https://registry-1.docker.io/v2/{REPOSITORY}/blobs/{key}": ["blob", key] for key in fixture.blobs})
    log, config = fixture.directory / "requests.jsonl", fixture.directory / "config.json"
    log.write_text("")
    config.write_text(json.dumps({"port": fixture.port, "curl": real_curl,
                                  "identities": identities, "log": str(log),
                                  "receiver_delay": fixture.receiver_delay}))
    environment = {key: value for key, value in os.environ.items()
                   if key != "BASH_ENV" and not key.lower().endswith("proxy")}
    environment.update({"PATH": f"{wrapper_dir}:{os.environ.get('PATH', '')}",
                        "HTTP_FIXTURE_CONFIG": str(config),
                        "HOST_FIXTURE_EXECUTED_MARKER": str(fixture.marker)})
    child = subprocess.Popen([str(extractor), str(fixture.pin), str(fixture.out)],
                             env=environment, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    try:
        stdout, stderr = child.communicate(timeout=20)
        status = child.returncode
    except subprocess.TimeoutExpired:
        os.killpg(child.pid, signal.SIGKILL)
        stdout, stderr = child.communicate()
        status = 124
    records = [json.loads(line) for line in log.read_text().splitlines()]
    return status, stdout, stderr, records


def success_reasons(fixture, status, stdout):
    reasons = []
    host = fixture.out / "rootfs" / fixture.host_path
    if status != 0:
        reasons.append(f"exit={status} (want 0)")
    if stdout != f"{host}\n".encode():
        reasons.append("stdout is not exactly the host path")
    expected = [(fixture.out / "manifest.json", fixture.manifest), (host, HOST_BYTES)]
    expected += [(fixture.out / f"layer-{index}.tar.gz", body) for index, body in enumerate(fixture.layers, 1)]
    if len(fixture.layers) == 2:
        expected.append((fixture.out / "rootfs/EXTRA/second.txt", b"distinct second-layer bytes\n"))
    for path, body in expected:
        if not path.is_file() or path.read_bytes() != body:
            reasons.append(f"wrong or missing fixture bytes: {path.name}")
    if not host.is_file() or not os.access(host, os.X_OK):
        reasons.append("host is not a regular executable")
    return reasons


def input_reasons(fixture, records, case: CaseSpec):
    # Expectations come only from the immutable case, never constructor defaults.
    expected_body = b" " * (case.token_length - len(EXPECTED_TOKEN_JSON)) + EXPECTED_TOKEN_JSON
    generated = fixture.stream_body if case.mode in ("declared", "chunked") else fixture.token_body
    reasons = []
    if len(generated) != case.token_length:
        reasons.append(f"generated token body length={len(generated)} (want {case.token_length})")
    try:
        if json.loads(generated) != {"token": "loopback-token-sentinel"}:
            reasons.append("generated token JSON identity differs from the case")
    except (ValueError, UnicodeError):
        reasons.append("generated token body is not complete valid JSON")
    if not fixture.transfers:
        reasons.append("no complete token input was recorded by the fixture")
    elif any(transfer["intended"] != case.token_length for transfer in fixture.transfers):
        lengths = [transfer["intended"] for transfer in fixture.transfers]
        reasons.append(f"served complete token lengths={lengths} (want {case.token_length})")
    if any(transfer["body_digest"] != digest(expected_body) for transfer in fixture.transfers):
        reasons.append("served complete token bytes differ from the requested JSON input")
    for transfer in fixture.transfers:
        headers = transfer["headers"]
        if case.mode == "chunked":
            if headers.get("transfer-encoding") != ["chunked"] or "content-length" in headers:
                reasons.append("emitted token framing is not chunked without Content-Length")
        elif headers.get("content-length") != [str(case.token_length)] or "transfer-encoding" in headers:
            reasons.append("emitted token Content-Length framing differs from the case")
    if case.want_success:
        complete = [transfer for transfer in fixture.transfers
                    if transfer["status"] == 200 and not transfer["partial"]]
        if not complete:
            reasons.append("no successful complete token transfer was observed")
        for transfer in complete:
            # Only successful complete responses must send the whole body. Socket
            # totals remain diagnostic for intentionally rejected oversized input.
            if transfer["sent"] != case.token_length:
                reasons.append(f"observed token body length={transfer['sent']} (want {case.token_length})")
            if transfer["observed_body_digest"] != digest(expected_body):
                reasons.append("observed token body digest differs from the requested complete input")
    if fixture.mode != case.mode or fixture.phase != case.phase:
        reasons.append("effective fixture mode/phase differs from the case")
    if len(fixture.layers) != (2 if case.two_layers else 1):
        reasons.append("effective layer count differs from the case")
    if fixture.receiver_delay != case.receiver_delay:
        reasons.append(f"effective receiver delay={fixture.receiver_delay} (want {case.receiver_delay})")
    token_records = [record for record in records if record.get("phase") == "token"]
    if not token_records:
        reasons.append("no executed token request was measured")
    for record in token_records:
        if record["receiver_delay_seconds"] != case.receiver_delay:
            reasons.append(f"executed receiver delay={record['receiver_delay_seconds']} (want {case.receiver_delay})")
        if record["receiver_delayed"] != bool(case.receiver_delay):
            reasons.append("requested receiver scheduling delay was not executed as specified")
        if record["observed_receiver_delay_seconds"] < case.receiver_delay:
            reasons.append(f"observed receiver pause={record['observed_receiver_delay_seconds']:.6f}s "
                           f"(want at least {case.receiver_delay}s)")
    return reasons


def case_reasons(fixture, result, case: CaseSpec):
    status, stdout, stderr, records = result
    mode, phase = case.mode, case.phase
    reasons = input_reasons(fixture, records, case)
    original = [row for row in fixture.requests if not row[2]]
    counts = {stage: sum(row[0] == stage for row in original) for stage in ("token", "manifest", "blob")}
    if fixture.token.encode() in stdout + stderr:
        reasons.append("bearer token value was printed")
    if re.search(rb"authorization\s*:\s*bearer", stdout + stderr, re.I):
        reasons.append("authorization header was printed")
    if fixture.marker.exists():
        reasons.append("fixture host was executed")
    if any(not row[3] or not row[4] for row in fixture.requests):
        reasons.append("request identity or recovered bearer token differs")
    if any("contract_error" in record for record in records):
        reasons.append("unexpected original request identity")
    if status < 0:
        reasons.append(f"exit={status}: abnormal signal termination is not a handled rejection")
    oversized = mode in ("declared", "chunked") and not case.want_success
    if not case.want_success:
        if status == 0 or status in (124, 137):
            reasons.append(f"exit={status} (want bounded nonzero rejection)")
        if stdout:
            reasons.append("host path emitted on rejection")
        if (fixture.out / "rootfs" / fixture.host_path).exists():
            reasons.append("host extracted despite rejection")
        expected = {"token": (1, 0, 0), "manifest": (1, 1, 0), "blob": (1, 1, 1)}[phase]
        if oversized:
            if not 1 <= counts["token"] <= 3 or counts["manifest"] or counts["blob"]:
                reasons.append(f"oversized token advanced to later phases: {counts}")
            if case.token_length == TOKEN_CEILING + 1 and counts["token"] != 1:
                reasons.append("cap-plus-one token must fail immediately without retries")
            if b"token" not in stderr.lower() or not re.search(
                    rb"exceed|too\s+(large|big)|limit|ceiling|\bcap\b", stderr, re.I):
                reasons.append("token size rejection diagnostic is missing")
        elif tuple(counts.values()) != expected:
            reasons.append(f"HTTP failure request counts={tuple(counts.values())}, want {expected}")
    else:
        reasons += success_reasons(fixture, status, stdout)
        expected = (2 if mode == "partial" else 1, 1, 2 if case.two_layers else 1)
        if tuple(counts.values()) != expected:
            reasons.append(f"request counts={tuple(counts.values())}, want {expected}")
    if oversized:
        # TCP writes can finish into socket buffers while the receiver is
        # descheduled. Server sent/BrokenPipe totals are diagnostics only.
        token_records = [record for record in records if record.get("phase") == "token"]
        if not token_records:
            reasons.append("no executed token transfer measured")
        for record in token_records:
            # The byte counter observes only body stdout, not output files or
            # write-out metadata. An unobserved sink cannot prove a receive-time
            # bound, even if the extractor later rejects its size correctly.
            if record["body_sink"] != "stdout":
                reasons.append(f"token body sink={record['body_sink']} is unobserved; receive bound is not verified")
            # Successful writes can lead the consumer by one measured pipe
            # buffer. The receive bound itself remains unchanged.
            elif record["forwarded"] > TOKEN_CEILING + record["pipe_capacity"]:
                reasons.append(f"forwarded token bytes={record['forwarded']} exceed ceiling plus measured pipe buffer")
            limit = record["max_filesize"]
            if limit is not None:
                parsed = re.fullmatch(r"(\d+)([KMGkmg]?)", limit)
                maximum = int(parsed[1]) * {"": 1, "k": 1024, "m": 1024 ** 2, "g": 1024 ** 3}[parsed[2].lower()] if parsed else 0
                if not 0 < maximum <= TOKEN_CEILING:
                    reasons.append("explicit token reception ceiling exceeds 1 MiB or is invalid")
    if mode == "partial" and not any(transfer["partial"] for transfer in fixture.transfers):
        reasons.append("first token response was not a partial-body failure")
    if mode == "redirect":
        redirects = [row for row in fixture.requests if row[2]]
        if len(redirects) != 1 or redirects[0][0] != phase:
            reasons.append("redirect destination was not fetched exactly once")
    if mode in ("normal", "401", "redirect"):
        for record in records:
            if "phase" in record and (mode == "normal" or record["phase"] == phase):
                if not record["fail"]:
                    reasons.append("executed --fail/--fail-with-body option is missing")
                if not record["location"]:
                    reasons.append("executed --location option is missing")
    return reasons


CASES = [
    # name, mode, phase, two layers, complete token bytes, delay seconds, success
    CaseSpec("r3-http-normal-token-success-exact-bytes", "normal", "token", False,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r3-http-oversized-token-with-length-rejected-during-reception", "declared", "token", False,
             2097152, 0, False),
    CaseSpec("r3-http-oversized-token-chunked-rejected-during-reception", "chunked", "token", False,
             2097152, 0, False),
    CaseSpec("r3-http-transient-truncated-token-recovers-with-exact-bytes", "partial", "token", False,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r7-http-two-layer-exact-urls-and-tokens", "normal", "token", True,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r7-http-token-401-fails-with-fail-option", "401", "token", True,
             len(EXPECTED_TOKEN_JSON), 0, False),
    CaseSpec("r7-http-manifest-401-fails-with-fail-option", "401", "manifest", True,
             len(EXPECTED_TOKEN_JSON), 0, False),
    CaseSpec("r7-http-blob-401-fails-with-fail-option", "401", "blob", True,
             len(EXPECTED_TOKEN_JSON), 0, False),
    CaseSpec("r7-http-token-redirect-followed-with-location", "redirect", "token", True,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r7-http-manifest-redirect-followed-with-location", "redirect", "manifest", True,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r7-http-blob-redirect-followed-with-location", "redirect", "blob", True,
             len(EXPECTED_TOKEN_JSON), 0, True),
    CaseSpec("r9-http-declared-rejection-survives-delayed-receiver", "declared", "token", False,
             2097152, 0.25, False),
    CaseSpec("r9-http-chunked-rejection-survives-delayed-receiver", "chunked", "token", False,
             2097152, 0.25, False),
    CaseSpec("r10-http-token-cap-minus-one-declared-succeeds", "declared", "token", False,
             1048575, 0, True),
    CaseSpec("r10-http-token-exact-cap-declared-succeeds", "declared", "token", False,
             1048576, 0, True),
    CaseSpec("r10-http-token-cap-plus-one-declared-is-rejected", "declared", "token", False,
             1048577, 0, False),
    CaseSpec("r10-http-token-cap-minus-one-chunked-succeeds", "chunked", "token", False,
             1048575, 0, True),
    CaseSpec("r10-http-token-exact-cap-chunked-succeeds", "chunked", "token", False,
             1048576, 0, True),
    CaseSpec("r10-http-token-cap-plus-one-chunked-is-rejected", "chunked", "token", False,
             1048577, 0, False),
]


def main():
    if Path(sys.argv[0]).name == "curl":
        return curl_wrapper(sys.argv[1:])
    extractor = Path(os.environ.get("HOST_IMAGE_EXTRACTOR", Path(__file__).with_name("extract-host-image.sh")))
    real_curl = shutil.which("curl")
    if not real_curl or not extractor.is_file():
        print("FAIL: real-http-harness-setup: curl or extractor is unavailable", flush=True)
        return 1
    failures = 0
    for case in CASES:
        try:
            with tempfile.TemporaryDirectory(prefix="host-image-http-") as directory:
                fixture = RegistryFixture(directory, case.mode, case.phase, case.two_layers,
                                          token_length=case.token_length,
                                          receiver_delay=case.receiver_delay)
                with fixture:
                    result = run_extractor(fixture, extractor, real_curl)
                reasons = case_reasons(fixture, result, case)
        except Exception as error:
            # Exception bodies may contain raw response bytes; report type only.
            reasons = [f"fixture error: {type(error).__name__}"]
        if reasons:
            print(f"FAIL: {case.name}: {'; '.join(reasons)}", flush=True)
            failures += 1
        else:
            print(f"PASS: {case.name}", flush=True)
    return int(failures != 0)


if __name__ == "__main__":
    sys.exit(main())
