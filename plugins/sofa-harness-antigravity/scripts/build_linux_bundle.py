"""Build a self-contained, symlink-free Linux bundle for sofa-harness-antigravity.

Produces `<out>/sofa-harness-antigravity/{sofa-harness-antigravity, python/...}` using
`uv` standalone Linux CPython + manylinux wheels so bare containers such as `golang:1.27`
can launch `--entrypoint /.sofa/harness/sofa-harness-antigravity` without pre-installed Python.
"""

from __future__ import annotations

import argparse
import os
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

_ARCH_MAP = {
    "amd64": ("cpython-3.12.14-linux-x86_64-gnu", "x86_64-manylinux_2_28"),
    "arm64": ("cpython-3.12.14-linux-aarch64-gnu", "aarch64-manylinux_2_28"),
}

_LAUNCHER_BODY = """#!/bin/sh
set -eu
SELF_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
if [ -x "$SELF_DIR/python/bin/python3" ]; then
  exec "$SELF_DIR/python/bin/python3" -m sofa_harness_antigravity "$@"
fi
exec python3 -m sofa_harness_antigravity "$@"
"""


def _run(cmd: list[str], cwd: Path | None = None) -> None:
    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(
            f"command {' '.join(cmd)} failed (exit {proc.returncode}):\n{proc.stdout}\n{proc.stderr}"
        )


def _fs_path(p: Path) -> str:
    """Use Windows extended-length path prefix when needed to bypass 260-char MAX_PATH."""
    s = str(p.resolve())
    if os.name == "nt" and not s.startswith("\\\\?\\") and len(s) >= 240:
        if s.startswith("\\\\"):
            return "\\\\?\\UNC\\" + s[2:]
        return "\\\\?\\" + s
    return s


def _strip_symlinks_and_copy(src: Path, dst: Path) -> None:
    """Copy directory tree dereferencing symlinks so Windows/ZIP/container mounts remain clean."""
    if dst.exists():
        shutil.rmtree(_fs_path(dst))
    os.makedirs(_fs_path(dst), exist_ok=True)
    for root, dirs, files in os.walk(_fs_path(src), followlinks=False):
        dirs[:] = [d for d in dirs if d != "__pycache__"]
        rel = Path(root).relative_to(Path(_fs_path(src)))
        target_root = dst / rel
        os.makedirs(_fs_path(target_root), exist_ok=True)
        for d in dirs:
            dpath = Path(root) / d
            if dpath.is_symlink():
                continue
            os.makedirs(_fs_path(target_root / d), exist_ok=True)
        for f in files:
            if f.endswith((".pyc", ".pyo")):
                continue
            fpath = Path(root) / f
            if fpath.is_symlink():
                resolved = fpath.resolve()
                if resolved.is_file():
                    shutil.copy2(_fs_path(resolved), _fs_path(target_root / f))
            else:
                shutil.copy2(_fs_path(fpath), _fs_path(target_root / f))


def build_bundle(out_dir: Path, arch: str, include_sdk: bool = True) -> Path:
    if arch not in _ARCH_MAP:
        raise ValueError(f"unsupported arch {arch!r}; expected amd64 or arm64")

    uv_py_name, uv_platform = _ARCH_MAP[arch]
    plugin_root = Path(__file__).resolve().parent.parent
    bundle_root = out_dir / "sofa-harness-antigravity"
    if bundle_root.exists():
        shutil.rmtree(bundle_root)
    bundle_root.mkdir(parents=True, exist_ok=True)

    python_dest = bundle_root / "python"
    with tempfile.TemporaryDirectory(prefix="sofa-ag-bundle-") as tmp_str:
        tmp_dir = Path(tmp_str)
        py_install_dir = tmp_dir / "py"
        _run(
            [
                "uv",
                "python",
                "install",
                uv_py_name,
                "--no-bin",
                "--no-registry",
                "--install-dir",
                str(py_install_dir),
            ]
        )

        installed_root = py_install_dir / uv_py_name
        if not installed_root.is_dir():
            candidates = [p for p in py_install_dir.iterdir() if p.is_dir() and p.name.startswith("cpython-")]
            if not candidates:
                raise RuntimeError(f"failed to locate installed Python under {py_install_dir}")
            installed_root = candidates[0]

        _strip_symlinks_and_copy(installed_root, python_dest)

        # Ensure bin/python3 exists as a regular executable copy of bin/python3.12 (no symlinks).
        bin_dir = python_dest / "bin"
        py312 = bin_dir / "python3.12"
        py3 = bin_dir / "python3"
        if py312.exists() and not py3.exists():
            shutil.copy2(py312, py3)
        for exe in (py312, py3):
            if exe.exists():
                os.chmod(exe, 0o755)

        site_packages = python_dest / "lib" / "python3.12" / "site-packages"
        site_packages.mkdir(parents=True, exist_ok=True)

        deps = [
            "grpcio>=1.70.0",
            "grpcio-health-checking>=1.70.0",
            "protobuf>=7.36.2",
            "cryptography>=44.0.0",
        ]
        if include_sdk:
            deps.append("google-antigravity>=0.1.24")

        _run(
            [
                "uv",
                "pip",
                "install",
                "--python-platform",
                uv_platform,
                "--python-version",
                "3.12",
                "--target",
                str(site_packages),
                "--upgrade",
                *deps,
            ]
        )

        # Install sofa_harness_antigravity package directly into site-packages (no editable links).
        src_pkg = plugin_root / "src" / "sofa_harness_antigravity"
        pkg_dest = site_packages / "sofa_harness_antigravity"
        _strip_symlinks_and_copy(src_pkg, pkg_dest)

    launcher_path = bundle_root / "sofa-harness-antigravity"
    launcher_path.write_bytes(_LAUNCHER_BODY.replace("\r\n", "\n").encode("utf-8"))
    os.chmod(launcher_path, stat.S_IRWXU | stat.S_IRGRP | stat.S_IXGRP | stat.S_IROTH | stat.S_IXOTH)
    return bundle_root


def main() -> None:
    parser = argparse.ArgumentParser(description="Build self-contained Linux sofa-harness-antigravity bundle")
    parser.add_argument("--out", default="./dist", help="Output parent directory")
    parser.add_argument("--arch", default="amd64", choices=["amd64", "arm64"], help="Target Linux architecture")
    parser.add_argument(
        "--skip-sdk",
        action="store_true",
        help="Skip bundling google-antigravity SDK (useful for lightweight fake_scripted integration tests)",
    )
    args = parser.parse_args()

    bundle_root = build_bundle(Path(args.out).resolve(), args.arch, include_sdk=not args.skip_sdk)
    print(f"Built self-contained Linux bundle at {bundle_root}")


if __name__ == "__main__":
    main()
