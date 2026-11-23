#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "invoke-toolkit>=0.0.59",
# ]
# ///
"""Local release automation tasks."""

from __future__ import annotations

import shlex
from pathlib import Path

from invoke_toolkit import Context, script, task


def _build_wheels_command(
    version: str,
    input_dir: Path,
    output_dir: Path,
    readme: Path,
    require_all: bool,
) -> list[str]:
    command = [
        "python",
        "scripts/build_wheels.py",
        "--version",
        version,
        "--input-dir",
        str(input_dir),
        "--output-dir",
        str(output_dir),
        "--readme",
        str(readme),
    ]
    if require_all:
        command.append("--require-all")
    return command


def _run_build_wheels(
    ctx: Context,
    version: str,
    input_dir: Path,
    output_dir: Path,
    readme: Path,
    require_all: bool,
) -> None:
    ctx.run(
        " ".join(
            shlex.quote(part)
            for part in _build_wheels_command(
                version, input_dir, output_dir, readme, require_all
            )
        )
    )


@task(name="build-wheels")
def build_wheels(
    ctx: Context,
    version: str,
    input_dir: Path,
    output_dir: Path = Path("wheels"),
    readme: Path = Path("README.md"),
    require_all: bool = False,
) -> None:
    """Build Python wheels from release archives."""
    _run_build_wheels(ctx, version, input_dir, output_dir, readme, require_all)


@task(name="download-release-archives")
def download_release_archives(
    ctx: Context,
    tag: str,
    repo: str,
    output_dir: Path = Path("artifacts"),
) -> None:
    """Download release archives from a GitHub Enterprise release."""
    output_dir.mkdir(parents=True, exist_ok=True)
    command = [
        "gh",
        "release",
        "download",
        tag,
        "--repo",
        repo,
        "--pattern",
        "agentsview_*.tar.gz",
        "--pattern",
        "agentsview_*.zip",
        "--dir",
        str(output_dir),
        "--clobber",
    ]
    ctx.run(" ".join(shlex.quote(part) for part in command))


@task(name="smoke-wheel")
def smoke_wheel(ctx: Context, wheel: Path) -> None:
    """Run the agentsview console command from a wheel in an isolated uv environment."""
    command = ["uv", "tool", "run", "--from", str(wheel), "agentsview", "--version"]
    ctx.run(" ".join(shlex.quote(part) for part in command))


@task(name="publish-wheels")
def publish_wheels(
    ctx: Context,
    tag: str,
    repo: str,
    input_dir: Path = Path("artifacts"),
    output_dir: Path = Path("wheels"),
    readme: Path = Path("README.md"),
    download: bool = True,
    require_all: bool = True,
    smoke: bool = True,
) -> None:
    """Build, smoke-test, and attach wheels to a GitHub Enterprise release."""
    if download:
        download_release_archives(ctx, tag, repo, input_dir)

    version = tag.removeprefix("v")
    _run_build_wheels(ctx, version, input_dir, output_dir, readme, require_all)

    wheels = sorted(output_dir.glob("agentsview-*.whl"))
    if not wheels:
        raise RuntimeError(f"no wheels built in {output_dir}")
    if smoke:
        smoke_wheel(ctx, wheels[0])

    command = ["gh", "release", "upload", tag, "--repo", repo, "--clobber"]
    command.extend(str(wheel) for wheel in wheels)
    ctx.run(" ".join(shlex.quote(part) for part in command))


@task(name="remote-build-wheel")
def remote_build_wheel(
    ctx: Context,
    host: str,
    repo: str,
    ref: str,
    version: str,
    artifact_dir: Path = Path("artifacts"),
    output_dir: Path = Path("wheels"),
    goos: str = "linux",
    goarch: str = "amd64",
    readme: Path = Path("README.md"),
) -> None:
    """Build a release archive on an SSH host and turn it into a local wheel."""
    remote_archive = f"/tmp/agentsview_{version}_{goos}_{goarch}.tar.gz"
    remote_command = " && ".join(
        [
            "set -eu",
            "workdir=$(mktemp -d)",
            'trap \'rm -rf "$workdir"\' EXIT',
            f"git clone --depth 1 --branch {shlex.quote(ref)} {shlex.quote(repo)} \"$workdir\"",
            'cd "$workdir"',
            f"GOOS={shlex.quote(goos)} GOARCH={shlex.quote(goarch)} make VERSION={shlex.quote(version)} build-release",
            f"tar czf {shlex.quote(remote_archive)} agentsview",
        ]
    )
    ctx.run(f"ssh {shlex.quote(host)} {shlex.quote(remote_command)}")

    artifact_dir.mkdir(parents=True, exist_ok=True)
    ctx.run(
        f"scp {shlex.quote(f'{host}:{remote_archive}')} {shlex.quote(str(artifact_dir))}"
    )
    ctx.run(f"ssh {shlex.quote(host)} rm -f {shlex.quote(remote_archive)}")
    _run_build_wheels(ctx, version, artifact_dir, output_dir, readme, False)

@task()
def run_upstream(ctx: Context, port: int = 8081, revision: str = "latest") -> None:
    """Run upstream AgentsView with a separate data directory."""
    command = ["uvx", f"agentsview@{revision}", "serve", "--port", str(port)]
    ctx.run(
        " ".join(shlex.quote(part) for part in command),
        env={"AGENTSVIEW_DATA_DIR": str(Path("/tmp") / f"agentsview@{revision}")},
    )


script()
