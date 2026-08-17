from __future__ import annotations

import io
import subprocess
import tarfile
from pathlib import Path


def _write_release_archive(path: Path) -> None:
    with tarfile.open(path, "w:gz") as archive:
        data = b"agentsview binary"
        member = tarfile.TarInfo("agentsview")
        member.size = len(data)
        archive.addfile(member, io.BytesIO(data))


def test_build_wheels_task_builds_wheel_from_release_archive(tmp_path: Path) -> None:
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    _write_release_archive(artifacts / "agentsview_1.2.3_linux_amd64.tar.gz")
    wheels = tmp_path / "wheels"

    result = subprocess.run(
        [
            "uv",
            "run",
            "tasks.py",
            "build-wheels",
            "--version",
            "1.2.3",
            "--input-dir",
            str(artifacts),
            "--output-dir",
            str(wheels),
        ],
        cwd=Path(__file__).parent,
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert (wheels / "agentsview-1.2.3-py3-none-manylinux_2_28_x86_64.whl").is_file()
