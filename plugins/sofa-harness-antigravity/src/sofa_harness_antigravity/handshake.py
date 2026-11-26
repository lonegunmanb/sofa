"""hashicorp/go-plugin AutoMTLS handshake and gRPC server bootstrap.

Note: hashicorp/go-plugin's AutoMTLS client generates ECDSA P-521 client
certificates, whereas grpcio's bundled BoringSSL rejects P-521 signature
algorithms (NO_COMMON_SIGNATURE_ALGORITHMS). Hosting the TLS 1.2/1.3 ALPN h2
mTLS listener on Python's stdlib OpenSSL (`ssl.SSLContext`) and forwarding
loopback traffic over an internal ECDSA P-256 mTLS connection to `grpc.aio.server()`
preserves strict end-to-end AutoMTLS with unmodified hashicorp/go-plugin clients
and leaves zero plaintext ports inside the container.
"""

from __future__ import annotations

import asyncio
import base64
import datetime
import ipaddress
import logging
import os
import ssl
import sys
import tempfile
from dataclasses import dataclass
from typing import TextIO

import grpc
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from sofa.harness.v1 import harness_pb2_grpc

CORE_PROTOCOL_VERSION = 1
APP_PROTOCOL_VERSION = 1
MAGIC_COOKIE_KEY = "SOFA_HARNESS_PLUGIN"
MAGIC_COOKIE_VALUE = "sofa-harness-v1"
DEFAULT_MIN_PORT = 19500
MAX_GRPC_MESSAGE_BYTES = 256 * 1024 * 1024


class HandshakeError(RuntimeError):
    """Raised when go-plugin handshake preconditions are violated."""


@dataclass(slots=True)
class PreparedHandshake:
    """Validated AutoMTLS credentials and listener parameters."""

    protocol_version: int
    unix_socket_dir: str
    min_port: int
    max_port: int
    server_credentials: grpc.ServerCredentials
    ssl_context: ssl.SSLContext
    server_cert_der_b64: str


class ManagedGRPCServer:
    """Lifecycle wrapper coordinating the OpenSSL mTLS frontend and grpc.aio backend."""

    def __init__(
        self,
        grpc_server: grpc.aio.Server,
        tls_server: asyncio.AbstractServer,
        internal_port: int,
    ) -> None:
        self._grpc_server = grpc_server
        self._tls_server = tls_server
        self.internal_port = internal_port
        self._active_conns: set[asyncio.Task[None]] = set()

    def track_connection(self, task: asyncio.Task[None]) -> None:
        self._active_conns.add(task)
        task.add_done_callback(self._active_conns.discard)

    async def wait_for_termination(self) -> None:
        await self._grpc_server.wait_for_termination()

    async def stop(self, grace: float | None = None) -> None:
        self._tls_server.close()
        try:
            await self._tls_server.wait_closed()
        except Exception:
            pass
        for task in list(self._active_conns):
            task.cancel()
        if self._active_conns:
            await asyncio.gather(*self._active_conns, return_exceptions=True)
        await self._grpc_server.stop(grace=grace)


def scrub_plugin_env() -> None:
    """Remove SOFA_HARNESS_PLUGIN and all PLUGIN_* variables from os.environ."""
    for key in list(os.environ.keys()):
        if key == MAGIC_COOKIE_KEY or key.startswith("PLUGIN_"):
            os.environ.pop(key, None)


def _negotiate_protocol_version(raw: str, default_version: int = APP_PROTOCOL_VERSION) -> int:
    if not raw.strip():
        return default_version
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            if int(part) == default_version:
                return default_version
        except ValueError:
            continue
    return default_version


def _load_cert_chain_from_pem(ctx: ssl.SSLContext, cert_pem: bytes, key_pem: bytes) -> None:
    cert_file = None
    key_file = None
    try:
        cert_file = tempfile.NamedTemporaryFile("wb", delete=False)
        cert_file.write(cert_pem)
        cert_file.close()

        key_file = tempfile.NamedTemporaryFile("wb", delete=False)
        key_file.write(key_pem)
        key_file.close()

        ctx.load_cert_chain(certfile=cert_file.name, keyfile=key_file.name)
    finally:
        for tmp_f in (cert_file, key_file):
            if tmp_f is not None:
                try:
                    os.unlink(tmp_f.name)
                except OSError:
                    pass


def _build_openssl_server_context(
    server_cert_pem: bytes,
    server_key_pem: bytes,
    client_cert_pem: bytes,
) -> ssl.SSLContext:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.set_alpn_protocols(["h2"])
    _load_cert_chain_from_pem(ctx, server_cert_pem, server_key_pem)
    try:
        ctx.load_verify_locations(cadata=client_cert_pem.decode("utf-8"))
    except (ssl.SSLError, UnicodeDecodeError) as exc:
        raise HandshakeError(f"invalid PLUGIN_CLIENT_CERT PEM: {exc}") from exc
    return ctx


def _build_openssl_client_context(
    ca_cert_pem: bytes,
    client_cert_pem: bytes,
    client_key_pem: bytes,
) -> ssl.SSLContext:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.set_alpn_protocols(["h2"])
    ctx.load_verify_locations(cadata=ca_cert_pem.decode("utf-8"))
    _load_cert_chain_from_pem(ctx, client_cert_pem, client_key_pem)
    return ctx


def _generate_p256_cert_pair(common_name: str = "localhost") -> tuple[bytes, bytes, bytes]:
    key = ec.generate_private_key(ec.SECP256R1())
    now = datetime.datetime.now(datetime.timezone.utc)
    subject = issuer = x509.Name(
        [
            x509.NameAttribute(NameOID.COMMON_NAME, common_name),
            x509.NameAttribute(NameOID.ORGANIZATION_NAME, "sofa"),
        ]
    )
    san = x509.SubjectAlternativeName(
        [
            x509.DNSName("localhost"),
            x509.IPAddress(ipaddress.ip_address("127.0.0.1")),
            x509.IPAddress(ipaddress.ip_address("0.0.0.0")),
        ]
    )
    cert = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(issuer)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(seconds=30))
        .not_valid_after(now + datetime.timedelta(hours=24))
        .add_extension(san, critical=False)
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .add_extension(
            x509.KeyUsage(
                digital_signature=True,
                key_encipherment=True,
                key_agreement=True,
                key_cert_sign=True,
                content_commitment=False,
                data_encipherment=False,
                crl_sign=False,
                encipher_only=False,
                decipher_only=False,
            ),
            critical=True,
        )
        .add_extension(
            x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH, ExtendedKeyUsageOID.CLIENT_AUTH]),
            critical=False,
        )
        .sign(key, hashes.SHA256())
    )

    cert_pem = cert.public_bytes(serialization.Encoding.PEM)
    key_pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    cert_der = cert.public_bytes(serialization.Encoding.DER)
    return cert_pem, key_pem, cert_der


def _generate_server_credentials(
    client_cert_pem: bytes,
) -> tuple[grpc.ServerCredentials, ssl.SSLContext, str]:
    server_cert_pem, server_key_pem, server_cert_der = _generate_p256_cert_pair("localhost")
    # hashicorp/go-plugin decodes with base64.RawStdEncoding (unpadded standard base64).
    cert_b64 = base64.b64encode(server_cert_der).decode("ascii").rstrip("=")

    grpc_creds = grpc.ssl_server_credentials(
        [(server_key_pem, server_cert_pem)],
        root_certificates=client_cert_pem,
        require_client_auth=True,
    )
    ssl_ctx = _build_openssl_server_context(server_cert_pem, server_key_pem, client_cert_pem)
    return grpc_creds, ssl_ctx, cert_b64


def prepare_handshake() -> PreparedHandshake:
    """Validate go-plugin env vars, generate ephemeral AutoMTLS certs, and scrub PLUGIN_* env."""
    cookie = os.environ.get(MAGIC_COOKIE_KEY, "")
    client_cert_raw = os.environ.get("PLUGIN_CLIENT_CERT", "")
    unix_socket_dir = os.environ.get("PLUGIN_UNIX_SOCKET_DIR", "").strip()
    min_port_raw = os.environ.get("PLUGIN_MIN_PORT", "").strip()
    max_port_raw = os.environ.get("PLUGIN_MAX_PORT", "").strip()
    proto_versions_raw = os.environ.get("PLUGIN_PROTOCOL_VERSIONS", "").strip()

    # Always scrub handshake secrets immediately so child processes never inherit them.
    scrub_plugin_env()

    if cookie != MAGIC_COOKIE_VALUE:
        raise HandshakeError(
            "This binary is a sofa harness plugin and must be launched by sofa (SOFA_HARNESS_PLUGIN mismatch)."
        )
    if not client_cert_raw.strip():
        raise HandshakeError("PLUGIN_CLIENT_CERT is required (AutoMTLS is mandatory).")

    try:
        min_port = int(min_port_raw) if min_port_raw else DEFAULT_MIN_PORT
        max_port = int(max_port_raw) if max_port_raw else min_port
    except ValueError as exc:
        raise HandshakeError(f"invalid PLUGIN_MIN_PORT/PLUGIN_MAX_PORT: {exc}") from exc

    if min_port < 0 or max_port > 65535 or max_port < min_port:
        raise HandshakeError(f"invalid port range {min_port}..{max_port}")

    negotiated = _negotiate_protocol_version(proto_versions_raw, APP_PROTOCOL_VERSION)
    creds, ssl_ctx, cert_b64 = _generate_server_credentials(client_cert_raw.encode("utf-8"))

    return PreparedHandshake(
        protocol_version=negotiated,
        unix_socket_dir=unix_socket_dir,
        min_port=min_port,
        max_port=max_port,
        server_credentials=creds,
        ssl_context=ssl_ctx,
        server_cert_der_b64=cert_b64,
    )


def configure_stderr_logging() -> None:
    """Redirect all Python logging strictly to stderr so stdout stays reserved for handshake."""
    logging.basicConfig(
        level=logging.WARNING,
        stream=sys.stderr,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        force=True,
    )


async def _pipe_stream(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
        if writer.can_write_eof():
            try:
                writer.write_eof()
            except Exception:
                pass
    except (asyncio.CancelledError, OSError, ssl.SSLError):
        pass


async def start_grpc_server(
    servicer: harness_pb2_grpc.AgentHarnessServicer,
    prepared: PreparedHandshake,
    stdout: TextIO | None = None,
) -> tuple[ManagedGRPCServer, str, str]:
    """Bind OpenSSL mTLS listener -> internal mTLS grpc.aio server, and emit go-plugin handshake."""
    out = stdout if stdout is not None else sys.stdout
    grpc_server = grpc.aio.server(
        options=[
            ("grpc.max_send_message_length", MAX_GRPC_MESSAGE_BYTES),
            ("grpc.max_receive_message_length", MAX_GRPC_MESSAGE_BYTES),
        ]
    )

    harness_pb2_grpc.add_AgentHarnessServicer_to_server(servicer, grpc_server)
    health_servicer = health.aio.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, grpc_server)
    await health_servicer.set("plugin", health_pb2.HealthCheckResponse.SERVING)
    await health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)

    # Enforce strict ECDSA P-256 mTLS on the internal loopback port too (zero plaintext ports).
    in_srv_cert, in_srv_key, _ = _generate_p256_cert_pair("localhost")
    in_cli_cert, in_cli_key, _ = _generate_p256_cert_pair("sofa-internal-proxy")
    internal_server_creds = grpc.ssl_server_credentials(
        [(in_srv_key, in_srv_cert)],
        root_certificates=in_cli_cert,
        require_client_auth=True,
    )
    internal_client_ssl_ctx = _build_openssl_client_context(
        ca_cert_pem=in_srv_cert,
        client_cert_pem=in_cli_cert,
        client_key_pem=in_cli_key,
    )

    internal_port = grpc_server.add_secure_port("127.0.0.1:0", internal_server_creds)
    if internal_port <= 0:
        raise HandshakeError("failed to bind internal loopback mTLS gRPC port")
    await grpc_server.start()

    managed_ref: list[ManagedGRPCServer] = []

    async def _handle_client(client_reader: asyncio.StreamReader, client_writer: asyncio.StreamWriter) -> None:
        upstream_writer: asyncio.StreamWriter | None = None
        try:
            upstream_reader, upstream_writer = await asyncio.open_connection(
                "127.0.0.1",
                internal_port,
                ssl=internal_client_ssl_ctx,
                server_hostname="localhost",
            )
            t1 = asyncio.create_task(_pipe_stream(client_reader, upstream_writer))
            t2 = asyncio.create_task(_pipe_stream(upstream_reader, client_writer))
            if managed_ref:
                managed_ref[0].track_connection(t1)
                managed_ref[0].track_connection(t2)
            await asyncio.gather(t1, t2, return_exceptions=True)
        except (asyncio.CancelledError, OSError, ssl.SSLError):
            pass
        finally:
            for w in (upstream_writer, client_writer):
                if w is not None:
                    try:
                        w.close()
                    except Exception:
                        pass

    tls_server: asyncio.AbstractServer | None = None
    if prepared.unix_socket_dir:
        sock_path = os.path.join(prepared.unix_socket_dir, "harness.sock").replace("\\", "/")
        if os.path.exists(sock_path):
            try:
                os.unlink(sock_path)
            except OSError:
                pass
        try:
            tls_server = await asyncio.start_unix_server(
                _handle_client,
                path=sock_path,
                ssl=prepared.ssl_context,
            )
        except OSError as exc:
            await grpc_server.stop(grace=0)
            raise HandshakeError(f"failed to bind unix mTLS listener {sock_path}: {exc}") from exc
        net_type = "unix"
        bound_addr = sock_path
    else:
        last_exc: OSError | None = None
        bound_port = 0
        for candidate in range(prepared.min_port, prepared.max_port + 1):
            try:
                tls_server = await asyncio.start_server(
                    _handle_client,
                    host="0.0.0.0",
                    port=candidate,
                    ssl=prepared.ssl_context,
                    reuse_address=True,
                )
                bound_port = tls_server.sockets[0].getsockname()[1]
                break
            except OSError as exc:
                last_exc = exc
        if tls_server is None or bound_port == 0:
            await grpc_server.stop(grace=0)
            raise HandshakeError(
                f"failed to bind AutoMTLS TCP port in range {prepared.min_port}..{prepared.max_port}: {last_exc}"
            )
        net_type = "tcp"
        bound_addr = f"0.0.0.0:{bound_port}"

    managed = ManagedGRPCServer(
        grpc_server=grpc_server,
        tls_server=tls_server,
        internal_port=internal_port,
    )
    managed_ref.append(managed)

    handshake_line = (
        f"{CORE_PROTOCOL_VERSION}|{prepared.protocol_version}|{net_type}|{bound_addr}|grpc|{prepared.server_cert_der_b64}\n"
    )
    out.write(handshake_line)
    out.flush()
    return managed, handshake_line, bound_addr
