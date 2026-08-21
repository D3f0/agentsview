from __future__ import annotations

import os
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


def _write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)


def test_publish_wheels_task_uploads_built_wheels_to_ghe_release(
    tmp_path: Path,
) -> None:
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    _write_release_archive(artifacts / "agentsview_1.2.3_linux_amd64.tar.gz")
    wheels = tmp_path / "wheels"
    commands = tmp_path / "commands"
    commands.mkdir()
    gh_log = tmp_path / "gh.log"
    _write_executable(
        commands / "gh",
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GH_LOG\"\n",
    )

    result = subprocess.run(
        [
            "uv",
            "run",
            "tasks.py",
            "publish-wheels",
            "--tag",
            "v1.2.3",
            "--repo",
            "github.ibm.com/team/agentsview",
            "--input-dir",
            str(artifacts),
            "--output-dir",
            str(wheels),
            "--no-download",
            "--no-smoke",
            "--no-require-all",
        ],
        cwd=Path(__file__).parent,
        env={
            **os.environ,
            "GH_LOG": str(gh_log),
            "PATH": f"{commands}:{os.environ['PATH']}",
        },
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    wheel = wheels / "agentsview-1.2.3-py3-none-manylinux_2_28_x86_64.whl"
    assert wheel.is_file()
    assert gh_log.read_text(encoding="utf-8") == (
        "release upload v1.2.3 --repo github.ibm.com/team/agentsview --clobber "
        f"{wheel}\n"
    )


def test_remote_build_wheel_task_fetches_remote_archive_and_builds_local_wheel(
    tmp_path: Path,
) -> None:
    fixture = tmp_path / "agentsview_1.2.3_linux_amd64.tar.gz"
    _write_release_archive(fixture)
    artifacts = tmp_path / "artifacts"
    wheels = tmp_path / "wheels"
    commands = tmp_path / "commands"
    commands.mkdir()
    ssh_log = tmp_path / "ssh.log"
    scp_log = tmp_path / "scp.log"
    _write_executable(
        commands / "ssh",
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SSH_LOG\"\n",
    )
    _write_executable(
        commands / "scp",
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SCP_LOG\"\n"
        "for destination; do :; done\n"
        "mkdir -p \"$destination\"\n"
        "cp \"$ARCHIVE_FIXTURE\" \"$destination/\"\n",
    )

    result = subprocess.run(
        [
            "uv",
            "run",
            "tasks.py",
            "remote-build-wheel",
            "--host",
            "builder.corp.example",
            "--repo",
            "https://github.ibm.com/team/agentsview.git",
            "--ref",
            "v1.2.3",
            "--version",
            "1.2.3",
            "--artifact-dir",
            str(artifacts),
            "--output-dir",
            str(wheels),
        ],
        cwd=Path(__file__).parent,
        env={
            **os.environ,
            "ARCHIVE_FIXTURE": str(fixture),
            "PATH": f"{commands}:{os.environ['PATH']}",
            "SCP_LOG": str(scp_log),
            "SSH_LOG": str(ssh_log),
        },
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert (wheels / "agentsview-1.2.3-py3-none-manylinux_2_28_x86_64.whl").is_file()
    assert "git clone --depth 1 --branch v1.2.3" in ssh_log.read_text(
        encoding="utf-8"
    )
    assert "GOOS=linux GOARCH=amd64 make VERSION=1.2.3 build-release" in ssh_log.read_text(
        encoding="utf-8"
    )
    assert "builder.corp.example:" in scp_log.read_text(encoding="utf-8")
