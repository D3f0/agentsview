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
    ctx.run(" ".join(shlex.quote(part) for part in command))


script()
