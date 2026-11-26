"""Offline unit tests for hashicorp/go-plugin AutoMTLS handshake."""

from __future__ import annotations

import base64
import datetime
import io
import ipaddress
import os
import sys

import grpc
import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID
from grpc_health.v1 import health_pb2, health_pb2_grpc

from sofa.harness.v1 import harness_pb2, harness_pb2_grpc
from sofa_harness_antigravity.handshake import (
    MAGIC_COOKIE_KEY,
    MAGIC_COOKIE_VALUE,
    HandshakeError,
    prepare_handshake,
    start_grpc_server,
)
from sofa_harness_antigravity.servicer import AgentHarnessServicer


def _generate_client_cert_pair(curve=None) -> tuple[bytes, bytes]:
    curve = curve or ec.SECP256R1()
    key = ec.generate_private_key(curve)
    now = datetime.datetime.now(datetime.timezone.utc)
    subject = issuer = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "sofa-test-client")])
    hash_alg = hashes.SHA512() if isinstance(curve, ec.SECP521R1) else hashes.SHA256()
    cert = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(issuer)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(seconds=30))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(key, hash_alg)
    )
    cert_pem = cert.public_bytes(serialization.Encoding.PEM)
    key_pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    return cert_pem, key_pem


def test_prepare_handshake_requires_magic_cookie(monkeypatch: pytest.MonkeyPatch) -> None:
    cert_pem, _ = _generate_client_cert_pair()
    monkeypatch.delenv(MAGIC_COOKIE_KEY, raising=False)
    monkeypatch.setenv("PLUGIN_CLIENT_CERT", cert_pem.decode("utf-8"))

    with pytest.raises(HandshakeError, match="SOFA_HARNESS_PLUGIN"):
        prepare_handshake()


def test_prepare_handshake_requires_client_cert(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
    monkeypatch.delenv("PLUGIN_CLIENT_CERT", raising=False)

    with pytest.raises(HandshakeError, match="PLUGIN_CLIENT_CERT"):
        prepare_handshake()


def test_prepare_handshake_scrubs_env_and_emits_unpadded_der(monkeypatch: pytest.MonkeyPatch) -> None:
    cert_pem, _ = _generate_client_cert_pair()
    monkeypatch.setenv(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
    monkeypatch.setenv("PLUGIN_CLIENT_CERT", cert_pem.decode("utf-8"))
    monkeypatch.setenv("PLUGIN_MIN_PORT", "19555")
    monkeypatch.setenv("PLUGIN_MAX_PORT", "19555")
    monkeypatch.setenv("PLUGIN_PROTOCOL_VERSIONS", "1")
    monkeypatch.setenv("PLUGIN_CUSTOM_SECRET", "must-be-scrubbed")
    monkeypatch.setenv("KEEP_UNRELATED_VAR", "still-here")

    prepared = prepare_handshake()

    # All PLUGIN_* and SOFA_HARNESS_PLUGIN keys MUST be scrubbed from os.environ.
    assert MAGIC_COOKIE_KEY not in os.environ
    for key in list(os.environ.keys()):
        assert not key.startswith("PLUGIN_"), f"{key} must be scrubbed"
    assert os.environ.get("KEEP_UNRELATED_VAR") == "still-here"

    # Base64 DER must be unpadded (RawStdEncoding compatible with hashicorp/go-plugin).
    assert "=" not in prepared.server_cert_der_b64
    padded = prepared.server_cert_der_b64 + "=" * (-len(prepared.server_cert_der_b64) % 4)
    der = base64.b64decode(padded, validate=True)
    parsed = x509.load_der_x509_certificate(der)
    san = parsed.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
    assert "localhost" in san.get_values_for_type(x509.DNSName)
    assert ipaddress.ip_address("127.0.0.1") in san.get_values_for_type(x509.IPAddress)


async def test_handshake_mtls_server_serves_health_and_rejects_rogue_client(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    client_cert_pem, client_key_pem = _generate_client_cert_pair()
    monkeypatch.setenv(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
    monkeypatch.setenv("PLUGIN_CLIENT_CERT", client_cert_pem.decode("utf-8"))
    monkeypatch.setenv("PLUGIN_MIN_PORT", "0")
    monkeypatch.delenv("PLUGIN_UNIX_SOCKET_DIR", raising=False)

    stdout_buf = io.StringIO()
    prepared = prepare_handshake()
    server, handshake_line, bound_addr = await start_grpc_server(
        servicer=AgentHarnessServicer(),
        prepared=prepared,
        stdout=stdout_buf,
    )
    try:
        line = stdout_buf.getvalue()
        assert line == handshake_line
        assert line.endswith("\n")
        parts = line.rstrip("\n").split("|")
        assert len(parts) == 6
        assert parts[0] == "1"
        assert parts[1] == "1"
        assert parts[2] == "tcp"
        assert parts[4] == "grpc"
        assert "=" not in parts[5]

        # Reconstruct server cert PEM from unpadded DER base64 in handshake line.
        padded = parts[5] + "=" * (-len(parts[5]) % 4)
        der = base64.b64decode(padded, validate=True)
        server_cert_pem = x509.load_der_x509_certificate(der).public_bytes(serialization.Encoding.PEM)

        port = bound_addr.rsplit(":", 1)[1]
        target = f"127.0.0.1:{port}"

        # Trusted mTLS client succeeds for Health("plugin") and GetCapabilities.
        trusted_creds = grpc.ssl_channel_credentials(
            root_certificates=server_cert_pem,
            private_key=client_key_pem,
            certificate_chain=client_cert_pem,
        )
        async with grpc.aio.secure_channel(
            target,
            trusted_creds,
            options=[("grpc.ssl_target_name_override", "localhost")],
        ) as chan:
            health_stub = health_pb2_grpc.HealthStub(chan)
            health_resp = await health_stub.Check(health_pb2.HealthCheckRequest(service="plugin"))
            assert health_resp.status == health_pb2.HealthCheckResponse.SERVING

            harness_stub = harness_pb2_grpc.AgentHarnessStub(chan)
            caps = await harness_stub.GetCapabilities(harness_pb2.GetCapabilitiesRequest())
            assert caps.harness_name == "antigravity"
            assert caps.protocol_version == 1

        # Rogue client without valid client key/cert MUST fail mTLS handshake.
        rogue_creds = grpc.ssl_channel_credentials(root_certificates=server_cert_pem)
        async with grpc.aio.secure_channel(
            target,
            rogue_creds,
            options=[("grpc.ssl_target_name_override", "localhost")],
        ) as rogue_chan:
            rogue_stub = harness_pb2_grpc.AgentHarnessStub(rogue_chan)
            with pytest.raises(grpc.aio.AioRpcError):
                await rogue_stub.GetCapabilities(harness_pb2.GetCapabilitiesRequest(), timeout=2.0)

        # Insecure channel MUST also fail.
        async with grpc.aio.insecure_channel(target) as insecure_chan:
            insecure_stub = harness_pb2_grpc.AgentHarnessStub(insecure_chan)
            with pytest.raises(grpc.aio.AioRpcError):
                await insecure_stub.GetCapabilities(harness_pb2.GetCapabilitiesRequest(), timeout=2.0)

        # Internal loopback port MUST also enforce strict mTLS (never plaintext).
        internal_target = f"127.0.0.1:{server.internal_port}"
        async with grpc.aio.insecure_channel(internal_target) as internal_insecure:
            internal_stub = harness_pb2_grpc.AgentHarnessStub(internal_insecure)
            with pytest.raises(grpc.aio.AioRpcError):
                await internal_stub.GetCapabilities(harness_pb2.GetCapabilitiesRequest(), timeout=2.0)
        async with grpc.aio.secure_channel(
            internal_target,
            rogue_creds,
            options=[("grpc.ssl_target_name_override", "localhost")],
        ) as internal_rogue:
            internal_rogue_stub = harness_pb2_grpc.AgentHarnessStub(internal_rogue)
            with pytest.raises(grpc.aio.AioRpcError):
                await internal_rogue_stub.GetCapabilities(harness_pb2.GetCapabilitiesRequest(), timeout=2.0)
    finally:
        await server.stop(grace=0.5)


async def test_handshake_accepts_p521_client_cert_matching_go_plugin_mtls(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import asyncio
    import ssl
    import tempfile

    client_cert_pem, client_key_pem = _generate_client_cert_pair(ec.SECP521R1())
    monkeypatch.setenv(MAGIC_COOKIE_KEY, MAGIC_COOKIE_VALUE)
    monkeypatch.setenv("PLUGIN_CLIENT_CERT", client_cert_pem.decode("utf-8"))
    monkeypatch.setenv("PLUGIN_MIN_PORT", "0")
    monkeypatch.delenv("PLUGIN_UNIX_SOCKET_DIR", raising=False)

    stdout_buf = io.StringIO()
    prepared = prepare_handshake()
    server, _, bound_addr = await start_grpc_server(
        servicer=AgentHarnessServicer(),
        prepared=prepared,
        stdout=stdout_buf,
    )
    try:
        b64 = stdout_buf.getvalue().rstrip("\n").split("|")[5]
        der = base64.b64decode(b64 + "=" * (-len(b64) % 4), validate=True)
        server_cert_pem = x509.load_der_x509_certificate(der).public_bytes(serialization.Encoding.PEM)

        cctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        cctx.load_verify_locations(cadata=server_cert_pem.decode("utf-8"))
        cctx.set_alpn_protocols(["h2"])
        with tempfile.NamedTemporaryFile("wb", delete=False) as cf, tempfile.NamedTemporaryFile("wb", delete=False) as kf:
            cf.write(client_cert_pem)
            cf.close()
            kf.write(client_key_pem)
            kf.close()
            try:
                cctx.load_cert_chain(cf.name, kf.name)
            finally:
                os.unlink(cf.name)
                os.unlink(kf.name)

        port = int(bound_addr.rsplit(":", 1)[1])
        _, writer = await asyncio.open_connection("127.0.0.1", port, ssl=cctx, server_hostname="localhost")
        assert writer.get_extra_info("ssl_object").selected_alpn_protocol() == "h2"
        writer.close()
    finally:
        await server.stop(grace=0.5)
