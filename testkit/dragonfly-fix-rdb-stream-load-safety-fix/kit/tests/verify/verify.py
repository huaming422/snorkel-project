"""Behavioral checks for stream loading in dragonfly.

Everything here goes through the RESP TCP server, which is the public surface
where the behavior is observable. Each test spawns the dragonfly binary on a
free port, opens a raw socket and drives it with hand-encoded RESP. Only
replies and server liveness are inspected, so the tests know nothing about
C++ identifiers, file layout or how the loader is structured internally.

A serialized value handed to RESTORE goes straight into the loader. The
malformed-stream cases build such a value by hand, mirroring the
serialization the server itself emits, so that the bytes decode cleanly yet
describe an inconsistent stream. The well-formed cases build their values the
honest way: XADD / XGROUP / XREADGROUP / XDEL, then DUMP, then RESTORE the
dump back.

Only the Python standard library plus pytest is imported, so the file runs
directly under the verifier interpreter with no repository code on the path.

Contract for each malformed case: RESTORE replies with an error, the key does
not exist afterwards (EXISTS -> 0), and the server still answers PING on a
fresh connection. Each test spawns its own server so a crash in one case
cannot bleed into another.

Two more load paths are driven without RESTORE. One writes a crafted snapshot
to disk and starts a server on it. The other points a server at a small
stand-in master over REPLICAOF, and that master answers the full sync with the
crafted value. In both cases the key must not exist once loading is over.
"""

from __future__ import annotations

import os
import socket
import struct
import subprocess
import threading
import time

import pytest


DRAGONFLY_BINARY = os.environ.get(
    "DRAGONFLY_PATH", "/repo/dragonfly/build-dbg/dragonfly"
)
HOST = "127.0.0.1"


# ── RDB / listpack serialization (stable repo format constants) ─────────────
# Every constant below is fixed by the on-disk RDB and listpack formats the
# server reads; none is a choice a fix could change. Sources in the repo:
#   RDB_TYPE_STREAM_LISTPACKS=15, RDB_SER_VERSION=9, length-encoding bit tags
#   (src/redis/rdb.h); CRC64-jones poly + reflected table, vector
#   crc64("123456789")==0xe9c6d914c4b8d9ca (src/redis/crc64.c); listpack
#   header LP_HDR_SIZE=6 / LP_EOF=0xFF and integer encodings
#   (src/redis/listpack.c); stream item flags (src/redis/stream.h).

RDB_TYPE_STREAM_LISTPACKS = 15
RDB_SER_VERSION = 9

STREAM_ITEM_FLAG_DELETED = 1
STREAM_ITEM_FLAG_SAMEFIELDS = 2


def _append_len(out: bytearray, value: int) -> None:
    """Packed unsigned length, mirroring WritePackedUInt (big-endian tags)."""
    if value < (1 << 6):
        out.append(value & 0xFF)  # RDB_6BITLEN
    elif value < (1 << 14):
        out.append(((value >> 8) & 0xFF) | (1 << 6))  # RDB_14BITLEN
        out.append(value & 0xFF)
    elif value <= 0xFFFFFFFF:
        out.append(0x80)  # RDB_32BITLEN
        out += struct.pack(">I", value)
    else:
        out.append(0x81)  # RDB_64BITLEN
        out += struct.pack(">Q", value)


def _append_string(out: bytearray, data: bytes) -> None:
    _append_len(out, len(data))
    out += data


# CRC64-jones: reflected, init 0, no final xor (matches src/redis/crc64.c).
def _crc64_table() -> list[int]:
    poly = 0x95AC9329AC4BC9B5  # reflect(0xad93d23594c935a9, 64)
    table = []
    for n in range(256):
        c = n
        for _ in range(8):
            c = (c >> 1) ^ poly if (c & 1) else (c >> 1)
        table.append(c & 0xFFFFFFFFFFFFFFFF)
    return table


_CRC64_TABLE = _crc64_table()


def _crc64(data: bytes, crc: int = 0) -> int:
    for b in data:
        crc = _CRC64_TABLE[(crc ^ b) & 0xFF] ^ (crc >> 8)
        crc &= 0xFFFFFFFFFFFFFFFF
    return crc


def _wrap_dump_footer(body: bytes) -> bytes:
    """Append the DUMP footer: 2-byte LE version + 8-byte LE CRC64."""
    out = bytearray(body)
    out += struct.pack("<H", RDB_SER_VERSION)
    out += struct.pack("<Q", _crc64(bytes(out)))
    return bytes(out)


# ── Minimal listpack encoder (integers only, enough for stream headers) ─────

def _lp_encode_integer(v: int) -> bytes:
    """Element bytes for an integer, mirroring lpEncodeIntegerGetType +
    the trailing back-length. The back-length encodes the element-encoding
    byte count (not counting the back-length itself)."""
    if 0 <= v <= 127:
        enc = bytes([v])
    elif -4096 <= v <= 4095:
        u = v + (1 << 13) if v < 0 else v
        enc = bytes([(u >> 8) | 0xC0, u & 0xFF])  # LP_ENCODING_13BIT_INT
    elif -32768 <= v <= 32767:
        u = v + (1 << 16) if v < 0 else v
        enc = bytes([0xF1, u & 0xFF, (u >> 8) & 0xFF])  # 16BIT
    elif -8388608 <= v <= 8388607:
        u = v + (1 << 24) if v < 0 else v
        enc = bytes([0xF2, u & 0xFF, (u >> 8) & 0xFF, (u >> 16) & 0xFF])  # 24BIT
    elif -2147483648 <= v <= 2147483647:
        u = v + (1 << 32) if v < 0 else v
        enc = bytes([0xF3, u & 0xFF, (u >> 8) & 0xFF, (u >> 16) & 0xFF, (u >> 24) & 0xFF])  # 32BIT
    else:
        u = v & 0xFFFFFFFFFFFFFFFF
        enc = bytes([0xF4]) + struct.pack("<Q", u)  # 64BIT
    return enc + _lp_encode_backlen(len(enc))


def _lp_encode_backlen(length: int) -> bytes:
    """Variable-length back-pointer, mirroring lpEncodeBacklen (7 bits/byte,
    high bit set on all but the most-significant, big-endian order)."""
    if length <= 127:
        return bytes([length])
    if length < 16383:
        return bytes([length >> 7, (length & 127) | 128])
    if length < 2097151:
        return bytes([length >> 14, ((length >> 7) & 127) | 128, (length & 127) | 128])
    if length < 268435455:
        return bytes([
            length >> 21,
            ((length >> 14) & 127) | 128,
            ((length >> 7) & 127) | 128,
            (length & 127) | 128,
        ])
    return bytes([
        length >> 28,
        ((length >> 21) & 127) | 128,
        ((length >> 14) & 127) | 128,
        ((length >> 7) & 127) | 128,
        (length & 127) | 128,
    ])


def _build_listpack(integers: list[int]) -> bytes:
    """A listpack whose elements are the given integers. Header is a 32-bit LE
    total-byte count, a 16-bit LE element count, then the elements, then LP_EOF.
    (LP_HDR_SIZE = 6.)"""
    body = bytearray()
    for v in integers:
        body += _lp_encode_integer(v)
    total = 6 + len(body) + 1  # header + body + LP_EOF
    out = bytearray()
    out += struct.pack("<I", total)
    out += struct.pack("<H", len(integers) & 0xFFFF)
    out += body
    out.append(0xFF)  # LP_EOF
    return bytes(out)


# ── Stream DUMP payload builders ────────────────────────────────────────────

def _build_cgroup_stream_dump(global_pel, consumers) -> bytes:
    """Stream value with an empty body and one consumer group whose global PEL
    and per-consumer PELs are exactly as given. ``global_pel`` is a list of
    16-byte ids; ``consumers`` is a list of (name, [16-byte ids]).

    Mirrors the serialization the loader reads: empty stream body, then the
    consumer-group block (name, last id, global PEL entries each with a
    delivery time + count, then consumers each with a seen time + their PEL
    id list)."""
    p = bytearray()
    p.append(RDB_TYPE_STREAM_LISTPACKS)
    _append_len(p, 0)  # listpack node count: empty body
    _append_len(p, 0)  # stream_len
    _append_len(p, 0)  # last_id.ms
    _append_len(p, 0)  # last_id.seq

    _append_len(p, 1)  # one consumer group
    _append_string(p, b"g")  # group name
    _append_len(p, 0)  # group last_id.ms
    _append_len(p, 0)  # group last_id.seq

    _append_len(p, len(global_pel))
    for pid in global_pel:
        p += pid  # 16 raw id bytes
        p += struct.pack("<Q", 0)  # delivery_time
        _append_len(p, 0)  # delivery_count

    _append_len(p, len(consumers))
    for name, pel in consumers:
        _append_string(p, name)
        p += struct.pack("<Q", 0)  # seen_time
        _append_len(p, len(pel))
        for pid in pel:
            p += pid

    return _wrap_dump_footer(bytes(p))


def _build_listpack_stream_dump(count, deleted, num_master_fields, record_flags=None,
                                lp_tail: bytes = b"") -> bytes:
    """One-node stream whose master-entry listpack declares the given counts.
    When ``record_flags`` is set (one flag value or a list of them), one
    record with no fields is appended per flag value, so the declared
    deleted-count can be made to disagree with the flags present.
    When ``lp_tail`` is set, those bytes and a second LP_EOF follow the
    listpack's own LP_EOF, and the header byte count covers them, so the
    listpack ends early inside its declared size.
    Mirrors the master-entry listpack layout:
    [valid-count, deleted-count, num-master-fields, 0 terminator, ...records]."""
    ints = [count, deleted, num_master_fields, 0]
    if record_flags is not None:
        assert num_master_fields == 0
        if isinstance(record_flags, int):
            record_flags = [record_flags]
        for flags in record_flags:
            ints += [flags, 0, 0, 3]  # flags, ms delta, seq delta, lp-count
    lp_blob = _build_listpack(ints)
    if lp_tail:
        lp_blob = bytearray(lp_blob + lp_tail + b"\xff")
        struct.pack_into("<I", lp_blob, 0, len(lp_blob))
        lp_blob = bytes(lp_blob)

    p = bytearray()
    p.append(RDB_TYPE_STREAM_LISTPACKS)
    _append_len(p, 1)  # one listpack node
    _append_string(p, b"\x01" * 16)  # 16-byte master id
    _append_string(p, lp_blob)
    _append_len(p, 0)  # stream_len
    _append_len(p, 0)  # last_id.ms
    _append_len(p, 0)  # last_id.seq
    _append_len(p, 0)  # consumer-group count

    return _wrap_dump_footer(bytes(p))


# ── RESP encoding + dragonfly process helpers ───────────────────────────────

def _encode_resp(*args: object) -> bytes:
    """Encode an array of bulk strings. ``bytes`` args pass through verbatim so
    binary DUMP payloads survive intact."""
    parts = []
    for a in args:
        parts.append(a if isinstance(a, (bytes, bytearray)) else str(a).encode())
    header = f"*{len(parts)}\r\n".encode()
    body = b"".join(b"$" + str(len(a)).encode() + b"\r\n" + bytes(a) + b"\r\n" for a in parts)
    return header + body


def _pick_free_port() -> int:
    s = socket.socket()
    try:
        s.bind(("", 0))
        return s.getsockname()[1]
    finally:
        s.close()


def _wait_until_listening(port: int, *, timeout_sec: float = 20.0) -> None:
    """Poll until dragonfly accepts a connection AND answers PING with PONG
    (dragonfly replies -LOADING to commands until its dataset load finishes)."""
    ping = b"*1\r\n$4\r\nPING\r\n"
    deadline = time.monotonic() + timeout_sec
    while time.monotonic() < deadline:
        try:
            s = socket.socket()
            s.settimeout(0.5)
            s.connect((HOST, port))
        except OSError:
            time.sleep(0.05)
            continue
        try:
            s.sendall(ping)
            s.settimeout(1.0)
            if s.recv(64).startswith(b"+PONG\r\n"):
                s.close()
                return
        except OSError:
            pass
        finally:
            try:
                s.close()
            except OSError:
                pass
        time.sleep(0.05)
    raise RuntimeError(
        f"dragonfly did not become command-ready on port {port} within {timeout_sec}s"
    )


def _spawn_dragonfly(port: int) -> subprocess.Popen:
    if not os.path.isfile(DRAGONFLY_BINARY) or not os.access(DRAGONFLY_BINARY, os.X_OK):
        raise RuntimeError(f"dragonfly binary missing or not executable at {DRAGONFLY_BINARY}")
    proc = subprocess.Popen(
        [
            DRAGONFLY_BINARY,
            f"--port={port}",
            "--proactor_threads=2",
            "--logtostderr",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        _wait_until_listening(port)
    except Exception:
        _kill(proc)
        raise
    return proc


def _kill(proc: subprocess.Popen, *, timeout_sec: float = 5.0) -> None:
    if proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=timeout_sec)
    except subprocess.TimeoutExpired:
        proc.kill()
        try:
            proc.wait(timeout=timeout_sec)
        except subprocess.TimeoutExpired:
            pass


@pytest.fixture
def dragonfly_port():
    port = _pick_free_port()
    proc = _spawn_dragonfly(port)
    try:
        yield port
    finally:
        _kill(proc)


def _read_reply(sock: socket.socket) -> bytes:
    """Read one complete RESP reply. Handles the reply types this verifier
    uses: simple string (+), error (-), integer (:), and bulk string ($),
    including a bulk payload (DUMP output) split across TCP segments or
    containing CRLF bytes."""
    buf = bytearray()

    def _recv_line() -> bytes:
        # Accumulate until the next CRLF; return the bytes up to and incl. it.
        while b"\r\n" not in bytes(buf):
            chunk = sock.recv(65536)
            if not chunk:
                return bytes(buf)
            buf.extend(chunk)
        idx = bytes(buf).index(b"\r\n") + 2
        line = bytes(buf[:idx])
        del buf[:idx]
        return line

    first = _recv_line()
    if not first or first[:1] != b"$":
        return first
    try:
        length = int(first[1:].strip())
    except ValueError:
        return first
    if length < 0:  # null bulk string ($-1)
        return first
    need = length + 2  # payload + trailing CRLF
    while len(buf) < need:
        chunk = sock.recv(65536)
        if not chunk:
            break
        buf.extend(chunk)
    return first + bytes(buf[:need])


def _command(port: int, *args: object, timeout_sec: float = 5.0) -> bytes:
    """Open a fresh connection, send one command, return the full reply.

    A fresh connection per command is deliberate: a malformed load may have
    crashed the server or dropped the prior connection, and a brand-new
    connect is the cleanest liveness probe."""
    s = socket.socket()
    s.settimeout(timeout_sec)
    s.connect((HOST, port))
    try:
        s.sendall(_encode_resp(*args))
        return _read_reply(s)
    finally:
        try:
            s.close()
        except OSError:
            pass


def _server_alive(port: int) -> bool:
    """A fresh-connection PING -> PONG proves the process survived the load."""
    try:
        return _command(port, "PING").startswith(b"+PONG\r\n")
    except OSError:
        return False


def _restore_is_rejected(port: int, key: str, payload: bytes) -> tuple[bool, bytes, bytes, bool]:
    """Drive one malformed RESTORE. Returns (rejected, restore_reply,
    exists_reply, alive). ``rejected`` means the whole rejection contract held:
    the RESTORE reply is not +OK, the key is absent, and the server is up."""
    try:
        restore_reply = _command(port, "RESTORE", key, "0", payload)
    except OSError as exc:
        # The connection dropped mid-RESTORE, so the load took the server down.
        # That is not a clean rejection.
        return (False, f"<connection error: {exc}>".encode(), b"<n/a>", _server_alive(port))

    alive = _server_alive(port)
    if not alive:
        return (False, restore_reply, b"<server down>", False)

    exists_reply = _command(port, "EXISTS", key)
    is_error = not restore_reply.startswith(b"+OK\r\n")
    not_created = exists_reply.startswith(b":0\r\n")
    return (is_error and not_created, restore_reply, exists_reply, alive)


# A 16-byte stream id, raw bytes as stored in the PEL.
_ID_A = b"A" * 16
_ID_B = b"B" * 16
_ID_C = b"C" * 16


# ── Malformed stream values are rejected on load ───────────────────────────

def _assert_all_rejected(port: int, label: str, cases) -> None:
    """RESTORE each (key, payload) pair and require the rejection contract for
    every one of them. The variants differ in key name and in the ids, names or
    counts they carry, so only the shape of the value decides the outcome."""
    for key, payload in cases:
        rejected, restore, exists, alive = _restore_is_rejected(port, key, payload)
        assert alive, (
            f"server did not survive loading a {label} stream (key {key!r}): "
            f"RESTORE reply={restore!r}"
        )
        assert rejected, (
            f"{label} stream was not safely rejected (key {key!r}): "
            f"RESTORE reply={restore!r} EXISTS={exists!r} server_alive={alive}"
        )


def test_pel_shared_entry_rejected(dragonfly_port: int):
    """Two consumers both claim the same global-PEL entry. RESTORE has to
    reply with an error, the key must not exist and the server keeps serving.
    The last variant has the second consumer claim a free entry first and the
    taken one after it."""
    _assert_all_rejected(dragonfly_port, "shared-PEL-entry", [
        ("shared", _build_cgroup_stream_dump(
            [_ID_A], [(b"c1", [_ID_A]), (b"c2", [_ID_A])])),
        ("pel-dup-owner", _build_cgroup_stream_dump(
            [_ID_B], [(b"alice", [_ID_B]), (b"bob", [_ID_B])])),
        ("pel-late-claim", _build_cgroup_stream_dump(
            [_ID_A, _ID_C], [(b"c1", [_ID_A]), (b"c2", [_ID_C, _ID_A])])),
    ])


def test_pel_unclaimed_entry_rejected(dragonfly_port: int):
    """A global-PEL entry that no consumer claims. RESTORE has to reply with
    an error, the key must not exist and the server keeps serving. The last
    variant claims one of two entries and leaves the other one ownerless."""
    _assert_all_rejected(dragonfly_port, "unclaimed-PEL-entry", [
        ("unclaimed", _build_cgroup_stream_dump([_ID_A], [(b"c1", [])])),
        ("pel-orphan", _build_cgroup_stream_dump([_ID_B], [(b"alice", [])])),
        ("pel-half-claimed", _build_cgroup_stream_dump(
            [_ID_A, _ID_B], [(b"c1", [_ID_A])])),
    ])


def test_listpack_fieldcount_overflow_rejected(dragonfly_port: int):
    """The master entry declares far more fields than the listpack holds.
    The server has to survive the RESTORE, reply with an error and leave no
    key behind."""
    _assert_all_rejected(dragonfly_port, "inflated-field-count", [
        ("overflow", _build_listpack_stream_dump(0, 0, 1000)),
        ("fields-3000", _build_listpack_stream_dump(0, 0, 3000)),
        ("fields-100000", _build_listpack_stream_dump(0, 0, 100000)),
    ])


def test_listpack_sum_overflow_rejected(dragonfly_port: int):
    """The master entry declares valid and deleted record counts that add up
    past INT64_MAX, far more records than the listpack holds. The server has
    to survive the RESTORE, reply with an error and leave no key behind."""
    _assert_all_rejected(dragonfly_port, "count-sum-overflow", [
        ("sumovf", _build_listpack_stream_dump((1 << 63) - 1, 1, 0)),
        ("sum-deleted-max", _build_listpack_stream_dump(1, (1 << 63) - 1, 0)),
        ("sum-halves", _build_listpack_stream_dump(1 << 62, 1 << 62, 0)),
    ])


def test_listpack_deleted_mismatch_rejected(dragonfly_port: int):
    """The declared deleted count disagrees with the deleted flags on the
    records present. RESTORE has to reply with an error, the key must not
    exist and the server keeps serving."""
    same = STREAM_ITEM_FLAG_SAMEFIELDS
    gone = STREAM_ITEM_FLAG_SAMEFIELDS | STREAM_ITEM_FLAG_DELETED
    _assert_all_rejected(dragonfly_port, "deleted-count-mismatch", [
        # declares 0 deleted, the one record is flagged deleted
        ("deleted-mismatch", _build_listpack_stream_dump(1, 0, 0, record_flags=gone)),
        # declares 1 deleted, the one record is live
        ("deleted-missing", _build_listpack_stream_dump(0, 1, 0, record_flags=[same])),
        # declares 1 deleted, both records are flagged deleted
        ("deleted-extra", _build_listpack_stream_dump(1, 1, 0, record_flags=[gone, gone])),
    ])


# ── The snapshot-file load path ──────────────────────────────────────────────
#
# A snapshot read at startup and a stream pushed by a master over replication
# both reach the server without RESTORE, and both are reachable on a
# no-authentication deployment. This case writes a crafted RDB snapshot to disk
# and starts a fresh server on it. Replication has its own tests below.
#
# The first case uses the deleted-count-mismatch value because it gives a
# deterministic, externally observable outcome on this path: the key is either
# stored or skipped, and the test asserts on that alone. The other crafted
# RESTORE values are not used here because their outcome on a startup load is
# not deterministic enough to assert on. The replication tests use the same
# value for the same reason. The second case is a listpack that ends early
# inside its declared size, which has the same stored-or-skipped outcome.


def _build_rdb_snapshot_file(dump_payload: bytes, key: str) -> bytes:
    """Wrap a stream value (the body of a DUMP payload) in a minimal legacy RDB
    file: the ``REDIS<ver>`` magic, a SELECTDB-0 opcode, one key/value pair, the
    EOF opcode, and the 8-byte trailing checksum. The DUMP payload is the object
    type byte, the serialized body, and a 10-byte footer (2-byte version +
    8-byte CRC); the RDB file reuses the type byte and body and supplies its own
    key string and file-level footer. The loader's checksum step reads but does
    not verify the trailing 8 bytes, so a zero checksum loads."""
    type_byte = dump_payload[0:1]
    body = dump_payload[1:-10]  # strip the DUMP type byte and 10-byte footer
    obj = bytearray()
    obj += type_byte
    _append_string(obj, key.encode())
    obj += body

    out = bytearray()
    out += b"REDIS%04d" % RDB_SER_VERSION  # magic + version, e.g. b"REDIS0009"
    out += b"\xfe\x00"                      # RDB_OPCODE_SELECTDB, db 0
    out += obj
    out += b"\xff"                          # RDB_OPCODE_EOF
    out += b"\x00" * 8                      # trailing checksum (read, not verified)
    return bytes(out)


def _spawn_dragonfly_with_snapshot(port: int, directory: str, dbfilename: str) -> subprocess.Popen:
    """Spawn a server that loads a snapshot from disk at startup. ``--dir`` +
    ``--dbfilename`` point it at the crafted file; the legacy RDB format is
    selected so the hand-built REDIS-magic file is the one that gets read."""
    if not os.path.isfile(DRAGONFLY_BINARY) or not os.access(DRAGONFLY_BINARY, os.X_OK):
        raise RuntimeError(f"dragonfly binary missing or not executable at {DRAGONFLY_BINARY}")
    return subprocess.Popen(
        [
            DRAGONFLY_BINARY,
            f"--port={port}",
            "--proactor_threads=2",
            f"--dir={directory}",
            f"--dbfilename={dbfilename}",
            "--df_snapshot_format=false",
            "--logtostderr",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def _wait_until_loaded(port: int, key: str, *, timeout_sec: float = 30.0) -> bytes:
    """Poll EXISTS until the startup load has finished. While the dataset is
    loading, non-PING commands reply ``-LOADING``; once the load completes
    (whether the crafted value was stored or rejected) the state flips to ACTIVE
    and EXISTS returns an integer. Returns that final EXISTS reply. Raises if the
    load never finishes (or the process died, dropping the connection)."""
    deadline = time.monotonic() + timeout_sec
    last: bytes = b"<no reply>"
    while time.monotonic() < deadline:
        try:
            reply = _command(port, "EXISTS", key)
        except OSError as exc:
            last = f"<connection error: {exc}>".encode()
            time.sleep(0.1)
            continue
        last = reply
        if reply[:1] == b":":  # integer reply -> load finished, state ACTIVE
            return reply
        time.sleep(0.1)
    raise RuntimeError(
        f"dragonfly did not finish loading on port {port} within {timeout_sec}s "
        f"(last EXISTS reply: {last!r})"
    )


def _load_snapshot(tmp_path, key: str, payload: bytes) -> tuple[bytes, bool]:
    """Write ``payload`` under ``key`` into a snapshot file, start a fresh
    server on it and return (EXISTS reply once loading is over, server alive)."""
    dbfilename = f"{key}.rdb"
    (tmp_path / dbfilename).write_bytes(_build_rdb_snapshot_file(payload, key))

    port = _pick_free_port()
    proc = _spawn_dragonfly_with_snapshot(port, str(tmp_path), dbfilename)
    try:
        exists_reply = _wait_until_loaded(port, key)
        alive = _server_alive(port)
    finally:
        _kill(proc)
    return exists_reply, alive


def test_snapshot_file_load_rejected(tmp_path):
    """The crafted value is loaded through the snapshot-file path, not RESTORE.
    The deleted-count-mismatch value must be skipped on this path too, so the
    key is never created (EXISTS -> 0) and the server comes up serving."""
    payload = _build_listpack_stream_dump(
        1, 0, 0, record_flags=STREAM_ITEM_FLAG_SAMEFIELDS | STREAM_ITEM_FLAG_DELETED
    )
    exists_reply, alive = _load_snapshot(tmp_path, "snapshot-dm", payload)

    assert alive, (
        "server did not survive loading a crafted stream snapshot at startup "
        "(it crashed during load)"
    )
    assert exists_reply.startswith(b":0\r\n"), (
        "a crafted stream value was stored through the snapshot-file load path: "
        f"the deleted-count-mismatch key was created (EXISTS={exists_reply!r})"
    )


def test_snapshot_trailing_bytes_rejected(tmp_path):
    """A one-entry stream whose listpack hits LP_EOF before the end of its
    declared size, with stray bytes after it, arrives in a snapshot file. Its
    counts and records are otherwise consistent. The key must not be created
    and the server comes up serving."""
    payload = _build_listpack_stream_dump(
        1, 0, 0, record_flags=STREAM_ITEM_FLAG_SAMEFIELDS, lp_tail=b"\x00\x01"
    )
    exists_reply, alive = _load_snapshot(tmp_path, "snapshot-tail", payload)

    assert alive, (
        "server did not survive loading a stream snapshot with bytes after the "
        "listpack end (it crashed during load)"
    )
    assert exists_reply.startswith(b":0\r\n"), (
        "a stream whose listpack ends early inside its declared size was stored "
        f"through the snapshot-file load path (EXISTS={exists_reply!r})"
    )


# ── The replication load path ───────────────────────────────────────────────
#
# A replica that full-syncs from a Redis-style master loads the snapshot it
# receives over the socket. To drive that, the test runs a stand-in master
# in-process and sends REPLICAOF to a fresh server. The stand-in answers the handshake and
# replies to PSYNC with FULLRESYNC plus a sized snapshot built the same way as
# in the file-path test.
#
# The replica gives one of two signals once it has dealt with the snapshot. A
# REPLCONF ACK means the full sync finished and it moved on to the command
# stream. A second PSYNC means it dropped the sync and started over. Either
# way loading is over, and the test then checks the key on the replica.


def _read_command(conn: socket.socket, buf: bytearray) -> list[bytes] | None:
    """Read one command from the replica, sent either as a RESP array or as an
    inline line. Returns its arguments, or None once the peer has closed."""

    def _fill(need: int) -> bool:
        while len(buf) < need:
            chunk = conn.recv(65536)
            if not chunk:
                return False
            buf.extend(chunk)
        return True

    def _line() -> bytes | None:
        while b"\r\n" not in buf:
            chunk = conn.recv(65536)
            if not chunk:
                return None
            buf.extend(chunk)
        end = buf.index(b"\r\n")
        line = bytes(buf[:end])
        del buf[: end + 2]
        return line

    first = _line()
    if first is None:
        return None
    if not first.startswith(b"*"):
        return first.split()
    args = []
    for _ in range(int(first[1:])):
        header = _line()
        if header is None:
            return None
        size = int(header[1:])
        if not _fill(size + 2):
            return None
        args.append(bytes(buf[:size]))
        del buf[: size + 2]
    return args


class _StandInMaster:
    """Plays a Redis master for one replica and hands it ``rdb`` on every
    full sync. It listens on a free local port and serves each connection on
    its own thread, because a replica that drops a sync reconnects."""

    def __init__(self, rdb: bytes):
        self._rdb = rdb
        self._cond = threading.Condition()
        self._psyncs = 0
        self._acked = False
        self._conns: list[socket.socket] = []
        self._listener = socket.socket()
        self._listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._listener.bind((HOST, 0))
        self._listener.listen(8)
        self.port = self._listener.getsockname()[1]
        threading.Thread(target=self._accept_loop, daemon=True).start()

    def _accept_loop(self) -> None:
        while True:
            try:
                conn, _ = self._listener.accept()
            except OSError:
                return
            self._conns.append(conn)
            threading.Thread(target=self._serve, args=(conn,), daemon=True).start()

    def _serve(self, conn: socket.socket) -> None:
        buf = bytearray()
        sent_snapshot = False
        try:
            while True:
                args = _read_command(conn, buf)
                if args is None:
                    return
                if not args:
                    continue
                name = args[0].upper()
                if name == b"PING":
                    conn.sendall(b"+PONG\r\n")
                elif name == b"PSYNC":
                    with self._cond:
                        self._psyncs += 1
                        self._cond.notify_all()
                    header = b"+FULLRESYNC " + b"0" * 40 + b" 0\r\n"
                    header += b"$%d\r\n" % len(self._rdb)
                    conn.sendall(header + self._rdb)
                    sent_snapshot = True
                elif name == b"REPLCONF" and len(args) > 1 and args[1].upper() == b"ACK":
                    # A master does not answer ACK. One that follows the
                    # snapshot means the replica finished loading it.
                    if sent_snapshot:
                        with self._cond:
                            self._acked = True
                            self._cond.notify_all()
                else:
                    conn.sendall(b"+OK\r\n")
        except (OSError, ValueError):
            return
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def wait_for_sync_result(self, timeout_sec: float) -> str | None:
        """Return "finished" once the replica ACKs after a snapshot, "dropped"
        once it asks for a second full sync, or None if neither happens."""
        with self._cond:
            self._cond.wait_for(lambda: self._acked or self._psyncs >= 2, timeout=timeout_sec)
            if self._acked:
                return "finished"
            if self._psyncs >= 2:
                return "dropped"
            return None

    def close(self) -> None:
        for s in [self._listener, *self._conns]:
            try:
                s.close()
            except OSError:
                pass


def _full_sync_from_stand_in(dump_payload: bytes, key: str) -> tuple[str | None, bytes, bool]:
    """Replicate a fresh server from a stand-in master that serves
    ``dump_payload`` under ``key``. Returns (sync result, EXISTS reply on the
    replica, replica still answers PING)."""
    master = _StandInMaster(_build_rdb_snapshot_file(dump_payload, key))
    port = _pick_free_port()
    proc = _spawn_dragonfly(port)
    try:
        reply = _command(port, "REPLICAOF", HOST, str(master.port), timeout_sec=20.0)
        assert reply.startswith(b"+OK\r\n"), f"REPLICAOF to the stand-in master failed: {reply!r}"
        result = master.wait_for_sync_result(timeout_sec=30.0)
        if result is None:
            return (None, b"<n/a>", _server_alive(port))
        exists_reply = _wait_until_loaded(port, key)
        return (result, exists_reply, _server_alive(port))
    finally:
        _kill(proc)
        master.close()


def test_replication_full_sync_rejected():
    """The deleted-count-mismatch value arrives as the snapshot of a full sync
    from a master. The replica must not store it: once the sync is finished or
    dropped, the key does not exist and the replica still serves."""
    key = "repl-dm"
    payload = _build_listpack_stream_dump(
        1, 0, 0, record_flags=STREAM_ITEM_FLAG_SAMEFIELDS | STREAM_ITEM_FLAG_DELETED
    )
    result, exists_reply, alive = _full_sync_from_stand_in(payload, key)

    assert alive, (
        "replica did not survive a full sync carrying a crafted stream value "
        "(it crashed during load)"
    )
    assert result is not None, (
        "replica neither finished nor dropped the full sync from the stand-in "
        "master within 30s"
    )
    assert exists_reply.startswith(b":0\r\n"), (
        "a crafted stream value was stored through a replication full sync: "
        f"the deleted-count-mismatch key was created (EXISTS={exists_reply!r}, "
        f"sync {result})"
    )


def test_replication_full_sync_wellformed_loads():
    """A valid stream with a single-owner consumer group, served the same way,
    must load on the replica: the full sync finishes and the key exists. This
    also shows the stand-in master gets the replica all the way through a
    full sync."""
    key = "repl-ok"
    payload = _build_cgroup_stream_dump([_ID_A], [(b"c1", [_ID_A])])
    result, exists_reply, alive = _full_sync_from_stand_in(payload, key)

    assert alive, "replica did not survive a full sync carrying a valid stream"
    assert result == "finished", (
        f"replica did not finish a full sync carrying a valid stream (sync {result})"
    )
    assert exists_reply.startswith(b":1\r\n"), (
        f"valid stream key missing on the replica after full sync (EXISTS={exists_reply!r})"
    )


# ── Well-formed streams load and round-trip ────────────────────────────────

def test_wellformed_consumer_group_roundtrips(dragonfly_port: int):
    """A hand-built stream with a valid single-owner consumer group must still
    load: RESTORE replies +OK and the key exists. Also check a two-consumer /
    two-entry group where each entry has exactly one owner."""
    port = dragonfly_port

    one = _build_cgroup_stream_dump([_ID_A], [(b"c1", [_ID_A])])
    reply = _command(port, "RESTORE", "ok1", "0", one)
    assert reply.startswith(b"+OK\r\n"), f"well-formed group rejected: {reply!r}"
    assert _command(port, "EXISTS", "ok1").startswith(b":1\r\n"), "well-formed group key missing"

    two = _build_cgroup_stream_dump([_ID_A, _ID_B], [(b"c1", [_ID_A]), (b"c2", [_ID_B])])
    reply = _command(port, "RESTORE", "ok2", "0", two)
    assert reply.startswith(b"+OK\r\n"), f"well-formed two-consumer group rejected: {reply!r}"
    assert _command(port, "EXISTS", "ok2").startswith(b":1\r\n"), "two-consumer group key missing"

    assert _server_alive(port), "server did not survive loading well-formed consumer groups"


def _dump_payload(port: int, key: str) -> bytes:
    """DUMP ``key`` and return the raw payload from the bulk-string reply."""
    dump = _command(port, "DUMP", key)
    # Parse the bulk-string DUMP reply: $<len>\r\n<payload>\r\n
    assert dump.startswith(b"$"), f"DUMP did not return a bulk string: {dump!r}"
    header, _, rest = dump.partition(b"\r\n")
    blob_len = int(header[1:])
    payload = rest[:blob_len]
    assert len(payload) == blob_len, "truncated DUMP payload"
    return payload


def test_wellformed_stream_roundtrips(dragonfly_port: int):
    """A real stream built with XADD, mixing entries that share a field set
    with entries that introduce new fields, must DUMP and RESTORE intact,
    preserving its length. A stream emptied with XDEL must round-trip too.
    Both exercise the server's own serialization through the same load path."""
    port = dragonfly_port

    # A stream that had an entry and lost it to XDEL. The server emits this
    # shape itself, so it has to load back with length 0.
    reply = _command(port, "XADD", "empty", "1-1", "a", "1")
    assert reply.startswith(b"$"), f"XADD failed: {reply!r}"
    assert _command(port, "XDEL", "empty", "1-1").startswith(b":1\r\n"), "XDEL failed"
    empty_payload = _dump_payload(port, "empty")
    assert _command(port, "DEL", "empty").startswith(b":1\r\n"), "DEL of the emptied key failed"
    reply = _command(port, "RESTORE", "empty", "0", empty_payload)
    assert reply.startswith(b"+OK\r\n"), f"RESTORE of an emptied stream failed: {reply!r}"
    assert _command(port, "XLEN", "empty").startswith(b":0\r\n"), "emptied stream lost its length"

    for args in (
        ("XADD", "s", "1-1", "a", "1", "b", "2"),   # fields {a, b}
        ("XADD", "s", "2-1", "a", "3", "b", "4"),   # same fields
        ("XADD", "s", "3-1", "c", "5"),             # new field set
    ):
        reply = _command(port, *args)
        assert reply.startswith(b"$") or reply.startswith(b"+"), f"XADD failed: {reply!r}"

    payload = _dump_payload(port, "s")
    assert _command(port, "DEL", "s").startswith(b":1\r\n"), "DEL of the dumped key failed"

    reply = _command(port, "RESTORE", "s", "0", payload)
    assert reply.startswith(b"+OK\r\n"), f"RESTORE of a real stream dump failed: {reply!r}"
    assert _command(port, "XLEN", "s").startswith(b":3\r\n"), "restored stream lost entries"

    assert _server_alive(port), "server did not survive the well-formed round-trip"


def test_wellformed_group_with_deleted_entry_roundtrips(dragonfly_port: int):
    """A real consumer group with pending entries spread over two consumers,
    plus one entry removed with XDEL while still pending, must DUMP and
    RESTORE intact. The node then carries a deleted-flagged record alongside
    live ones, and the group PEL references an id the stream body no longer
    holds. Both are shapes the server emits itself. After the round trip every
    pending id has to be acknowledgeable exactly once, which only works when
    each pending entry came back owned by its consumer."""
    port = dragonfly_port

    for args in (
        ("XADD", "g", "1-1", "a", "1", "b", "2"),
        ("XADD", "g", "2-1", "a", "3", "b", "4"),
        ("XADD", "g", "3-1", "c", "5"),
    ):
        reply = _command(port, *args)
        assert reply.startswith(b"$"), f"XADD failed: {reply!r}"
    assert _command(port, "XGROUP", "CREATE", "g", "grp", "0").startswith(b"+OK\r\n"), "XGROUP failed"
    reply = _command(port, "XREADGROUP", "GROUP", "grp", "c1", "COUNT", "2", "STREAMS", "g", ">")
    assert reply.startswith(b"*"), f"XREADGROUP for c1 failed: {reply!r}"
    reply = _command(port, "XREADGROUP", "GROUP", "grp", "c2", "COUNT", "1", "STREAMS", "g", ">")
    assert reply.startswith(b"*"), f"XREADGROUP for c2 failed: {reply!r}"
    assert _command(port, "XDEL", "g", "2-1").startswith(b":1\r\n"), "XDEL failed"

    payload = _dump_payload(port, "g")
    assert _command(port, "DEL", "g").startswith(b":1\r\n"), "DEL of the dumped key failed"

    reply = _command(port, "RESTORE", "g", "0", payload)
    assert reply.startswith(b"+OK\r\n"), f"RESTORE of a real group dump failed: {reply!r}"
    assert _command(port, "XLEN", "g").startswith(b":2\r\n"), "restored stream lost entries"

    for entry_id in ("1-1", "2-1", "3-1"):
        reply = _command(port, "XACK", "g", "grp", entry_id)
        assert reply.startswith(b":1\r\n"), f"pending entry {entry_id} did not survive: {reply!r}"
    reply = _command(port, "XACK", "g", "grp", "1-1")
    assert reply.startswith(b":0\r\n"), f"entry 1-1 acknowledged twice: {reply!r}"

    assert _server_alive(port), "server did not survive the consumer-group round-trip"
