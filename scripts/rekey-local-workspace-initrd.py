#!/usr/bin/env python3
"""Replace the one image-owned Agent public-key file in a gzip/newc initrd."""

import argparse
import base64
import gzip
import json
import os
import stat
import tempfile

_KEY_PATH = "etc/hako/workspace-agent-public-keys.json"
_HEADER = 110


def read_exact(stream, size):
    data = stream.read(size)
    if len(data) != size:
        raise ValueError("truncated initrd archive")
    return data


def padding(length):
    return b"\0" * ((-length) % 4)


def public_key(data):
    if len(data) > 256:
        raise ValueError("Agent public key file is oversized")
    try:
        value = json.loads(data)
        if not isinstance(value, dict) or set(value) != {"local_agent"}:
            raise ValueError
        encoded = value["local_agent"]
        if not isinstance(encoded, str) or len(encoded) != 43:
            raise ValueError
        key = base64.b64decode(encoded + "=", altchars=b"-_", validate=True)
        if len(key) != 32:
            raise ValueError
    except (ValueError, TypeError, KeyError, json.JSONDecodeError) as exc:
        raise ValueError("Agent public key file must contain one local_agent Ed25519 key") from exc
    return data


def rekey(source, destination, key_path, agent_path=None, guest_init_path=None):
    with open(key_path, "rb") as key_file:
        public_replacement = public_key(key_file.read(257))
    replacements = {_KEY_PATH: public_replacement}
    for path, archive_name, label in (
            (agent_path, "usr/local/bin/hako-workspace-agent", "Workspace Agent"),
            (guest_init_path, "init", "Guest init")):
        if path is None:
            continue
        replacement_stat = os.lstat(path)
        if (not stat.S_ISREG(replacement_stat.st_mode) or replacement_stat.st_mode & 0o111 == 0 or
                replacement_stat.st_mode & 0o022 or replacement_stat.st_uid != os.getuid() or
                replacement_stat.st_size < 1 or replacement_stat.st_size > 64 << 20):
            raise ValueError(f"{label} replacement must be an owned regular executable under 64 MiB")
        with open(path, "rb") as replacement_file:
            replacements[archive_name] = replacement_file.read((64 << 20) + 1)
    source_stat = os.lstat(source)
    if not stat.S_ISREG(source_stat.st_mode) or source_stat.st_mode & 0o022:
        raise ValueError("initrd must be a regular, non-group/world-writable file")
    fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    matches = set()
    try:
        with os.fdopen(fd, "wb") as raw_out, gzip.open(source, "rb") as raw_in:
            with gzip.GzipFile(fileobj=raw_out, mode="wb", compresslevel=1, mtime=0) as out:
                while True:
                    header = read_exact(raw_in, _HEADER)
                    if header[:6] not in (b"070701", b"070702"):
                        raise ValueError("unsupported initrd archive format")
                    fields = [int(header[index:index + 8], 16) for index in range(6, _HEADER, 8)]
                    mode, uid, gid, size, namesize = fields[1], fields[2], fields[3], fields[6], fields[11]
                    if namesize < 1 or namesize > 4096 or size > (1 << 30):
                        raise ValueError("invalid initrd archive entry bounds")
                    name_bytes = read_exact(raw_in, namesize)
                    name = name_bytes[:-1].decode("utf-8", "strict") if name_bytes[-1:] == b"\0" else ""
                    name_pad = padding(_HEADER + namesize)
                    read_exact(raw_in, len(name_pad))
                    data = read_exact(raw_in, size)
                    data_pad = padding(size)
                    read_exact(raw_in, len(data_pad))
                    if name in replacements:
                        if (name in matches or not stat.S_ISREG(mode) or uid != 0 or gid != 0 or
                                (name == _KEY_PATH and size != len(replacements[name])) or
                                (name != _KEY_PATH and mode & 0o111 == 0)):
                            raise ValueError("initrd Agent entry does not match the expected image-owned file")
                        matches.add(name)
                        data = replacements[name]
                        if len(data) != size:
                            fields[6] = len(data)
                            header = header[:6] + b"".join(f"{item:08x}".encode() for item in fields)
                    out.write(header)
                    out.write(name_bytes)
                    out.write(name_pad)
                    out.write(data)
                    out.write(padding(len(data)))
                    if name == "TRAILER!!!":
                        trailing = raw_in.read(512)
                        if (size != 0 or matches != set(replacements) or len(trailing) > 511 or trailing.strip(b"\0") or
                                raw_in.read(1)):
                            raise ValueError("initrd archive trailer or Agent key entry is invalid")
                        out.write(b"\0" * ((-out.tell()) % 512))
                        break
        os.chmod(destination, 0o600)
    except Exception:
        try:
            os.unlink(destination)
        except FileNotFoundError:
            pass
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--initrd", required=True)
    parser.add_argument("--agent-public-key", required=True)
    parser.add_argument("--workspace-agent-binary")
    parser.add_argument("--guest-init-binary")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    source = os.path.abspath(args.initrd)
    destination = os.path.abspath(args.output)
    if source == destination:
        parser.error("output must be a new path, separate from input")
    try:
        rekey(source, destination, args.agent_public_key, args.workspace_agent_binary, args.guest_init_binary)
    except (OSError, ValueError, UnicodeError, EOFError) as exc:
        parser.error(str(exc))
    print("LOCAL_WORKSPACE_INITRD_REKEY_OK")


if __name__ == "__main__":
    main()
