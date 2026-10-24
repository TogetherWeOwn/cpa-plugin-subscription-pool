#!/usr/bin/env python3
"""Stage an owner-supplied NVIDIA key privately; never activate or deploy it.

No network requests, management writes, inference probes, timers, or entitlement
assumptions. The staged JSON is a YAML-compatible *fragment*, not a full host
configuration. A release operator must GET/back up/merge/PUT the full management
value later, after validating account/model terms and deploying the provider.
"""

import argparse
import json
import os
import pathlib
import re
import secrets
import stat
import time

MAX_BYTES = 16384
STATUSES = {"waiting_for_key", "staged_not_activated", "already_staged_not_activated"}


class PickupError(Exception):
    """Closed-vocabulary errors only: never include key material or file content."""


def private_dir(path):
    if not path.is_absolute():
        raise PickupError("absolute_private_path_required")
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    info = os.fstat(fd)
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
        os.close(fd)
        raise PickupError("private_directory_required")
    return fd


def read_private(parent_fd, name):
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent_fd)
    except FileNotFoundError:
        return None
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
            raise PickupError("private_regular_file_required")
        value = os.read(fd, MAX_BYTES + 1)
        if len(value) > MAX_BYTES:
            raise PickupError("input_too_large")
        return value
    finally:
        os.close(fd)


def parse_key(raw):
    if raw is None:
        return None
    try:
        lines = [line.strip() for line in raw.decode("utf-8").splitlines() if line.strip() and not line.lstrip().startswith("#")]
    except UnicodeError:
        raise PickupError("invalid_key_file") from None
    if len(lines) != 1 or not lines[0].startswith("KEY="):
        raise PickupError("invalid_key_file")
    key = lines[0][4:]
    if not key or "placeholder" in key.lower():
        return None
    if not re.fullmatch(r"[A-Za-z0-9_.-]{20,8192}", key):
        raise PickupError("invalid_key_file")
    return key


def pickup(source, destination):
    source, destination = pathlib.Path(source), pathlib.Path(destination)
    source_fd = dest_fd = None
    temp = None
    try:
        source_fd = private_dir(source.parent)
        key = parse_key(read_private(source_fd, source.name))
        if key is None:
            return "waiting_for_key"
        dest_fd = private_dir(destination.parent)
        value = {"openai-compatibility": [{"name": "nvidia-nim", "base-url": "https://integrate.api.nvidia.com/v1", "api-key-entries": [{"api-key": key}], "models": []}]}
        existing = read_private(dest_fd, destination.name)
        if existing is not None:
            try:
                same = json.loads(existing) == value
            except (ValueError, UnicodeError):
                same = False
            if not same:
                raise PickupError("existing_stage_conflict")
            return "already_staged_not_activated"
        temp = ".nvidia-stage-" + secrets.token_hex(12)
        fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=dest_fd)
        with os.fdopen(fd, "wb") as output:
            output.write((json.dumps(value, indent=2) + "\n").encode())
            output.flush()
            os.fsync(output.fileno())
        # Hard-link creation is atomic and cannot replace an existing file.
        os.link(temp, destination.name, src_dir_fd=dest_fd, dst_dir_fd=dest_fd, follow_symlinks=False)
        os.fsync(dest_fd)
        return "staged_not_activated"
    except OSError:
        raise PickupError("private_file_io_error") from None
    finally:
        if temp is not None and dest_fd is not None:
            try:
                os.unlink(temp, dir_fd=dest_fd)
            except OSError:
                pass
        for fd in (source_fd, dest_fd):
            if fd is not None:
                os.close(fd)


def receipt(status):
    if status not in STATUSES:
        raise PickupError("invalid_status")
    return {"provider": "nvidia-nim", "status": status, "production_changed": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--key-file", type=pathlib.Path, default=pathlib.Path.home() / "secure-drop/nvidia.env")
    parser.add_argument("--output", type=pathlib.Path, default=pathlib.Path.home() / "secure-drop/nvidia.provider.json")
    parser.add_argument("--watch", action="store_true", help="wait in this process for the key; never installs a timer")
    parser.add_argument("--interval", type=int, default=30)
    args = parser.parse_args()
    if not 5 <= args.interval <= 3600:
        parser.error("interval must be between 5 and 3600 seconds")
    try:
        while True:
            status = pickup(args.key_file, args.output)
            if status != "waiting_for_key" or not args.watch:
                print(json.dumps(receipt(status), sort_keys=True))
                return 0
            time.sleep(args.interval)
    except PickupError as err:
        print(json.dumps({"provider": "nvidia-nim", "status": "refused", "error": str(err), "production_changed": False}, sort_keys=True))
        return 1
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
