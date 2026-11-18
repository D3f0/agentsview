import hashlib
import importlib.util
import json
from pathlib import Path

import pytest

spec = importlib.util.spec_from_file_location("bob_release", Path(__file__).with_name("bob_release.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
TAG = "v0.47.0-bob.2.2"
SOURCE = "8823ce27240abacf3ddac4214db70a266941f82e"


def artifacts(path):
    package = "0.47.0+bob.2.2"
    data = {
        f"agentsview_{package}_darwin_arm64.tar.gz": b"archive",
        f"agentsview-{package}-py3-none-macosx_13_0_arm64.whl": b"wheel",
        "BUILD.json": json.dumps({"tag": TAG, "source_sha": SOURCE, "package_version": package,
            "api_version": 15, "data_version": 169, "minos": "13.0", "architecture": "arm64"}).encode(),
    }
    for name, value in data.items():
        (path / name).write_bytes(value)
    (path / "SHA256SUMS").write_text("".join(f"{hashlib.sha256(value).hexdigest()}  {name}\n" for name, value in data.items()))


@pytest.mark.parametrize("tag,source", [("v0.47.0-bob.2.1", SOURCE), (TAG, "ef43f625"), ("v0.47.0", SOURCE), (TAG + "\nwrong", SOURCE)])
def test_rejects_immutable_or_ambiguous_target(tag, source):
    with pytest.raises(ValueError):
        module.release_parameters(tag, source)


def test_artifact_gate_accepts_exact_source_and_complete_hashes(tmp_path):
    artifacts(tmp_path)
    result = module.verify_artifacts(tmp_path, TAG, SOURCE)
    assert set(result) == {"SHA256SUMS", "BUILD.json", "agentsview_0.47.0+bob.2.2_darwin_arm64.tar.gz", "agentsview-0.47.0+bob.2.2-py3-none-macosx_13_0_arm64.whl"}


@pytest.mark.parametrize("mutation", ["stale", "tamper", "wrong-source", "wrong-version"])
def test_artifact_gate_rejects_stale_or_mismatched_inputs(tmp_path, mutation):
    artifacts(tmp_path)
    if mutation == "stale":
        (tmp_path / "old.whl").write_bytes(b"stale")
    elif mutation == "tamper":
        (tmp_path / "agentsview_0.47.0+bob.2.2_darwin_arm64.tar.gz").write_bytes(b"changed")
    else:
        build = json.loads((tmp_path / "BUILD.json").read_text())
        build["source_sha" if mutation == "wrong-source" else "data_version"] = "ef43f625641297f7818749c48760b46db1afab8a" if mutation == "wrong-source" else 92
        value = json.dumps(build).encode()
        (tmp_path / "BUILD.json").write_bytes(value)
        lines = (tmp_path / "SHA256SUMS").read_text().splitlines()
        lines = [f"{hashlib.sha256(value).hexdigest()}  BUILD.json" if line.endswith("  BUILD.json") else line for line in lines]
        (tmp_path / "SHA256SUMS").write_text("\n".join(lines) + "\n")
    with pytest.raises(ValueError):
        module.verify_artifacts(tmp_path, TAG, SOURCE)


def intel_artifacts(path, operating_system="darwin", architecture="amd64"):
    wheel_tag, suffix, compatibility = {
        ("darwin", "amd64"): ("macosx_13_0_x86_64", "tar.gz", {"minos": "13.0"}),
        ("linux", "amd64"): ("manylinux_2_28_x86_64", "tar.gz", {"glibc_max": "2.28"}),
        ("linux", "arm64"): ("manylinux_2_28_aarch64", "tar.gz", {"glibc_max": "2.28"}),
        ("windows", "amd64"): ("win_amd64", "zip", {}),
        ("windows", "arm64"): ("win_arm64", "zip", {}),
    }[(operating_system, architecture)]
    data = {
        f"agentsview_0.47.0+bob.2.2_{operating_system}_{architecture}.{suffix}": b"platform archive",
        f"agentsview-0.47.0+bob.2.2-py3-none-{wheel_tag}.whl": b"platform wheel",
        f"BUILD-{operating_system}-{architecture}.json": json.dumps({
            "tag": TAG, "source_sha": SOURCE, "package_version": "0.47.0+bob.2.2",
            "api_version": 15, "data_version": 169, **compatibility,
            "architecture": architecture, "workflow_sha": "a" * 40,
        }).encode(),
    }
    for name, value in data.items():
        (path / name).write_bytes(value)
    (path / f"SHA256SUMS-{operating_system}-{architecture}").write_text("".join(
        f"{hashlib.sha256(value).hexdigest()}  {name}\n" for name, value in data.items()))


@pytest.mark.parametrize("operating_system,architecture,archive", [
    ("linux", "arm64", "agentsview_0.47.0+bob.2.2_linux_arm64.tar.gz"),
    ("windows", "amd64", "agentsview_0.47.0+bob.2.2_windows_amd64.zip"),
    ("windows", "arm64", "agentsview_0.47.0+bob.2.2_windows_arm64.zip"),
])
def test_missing_platform_gate_verifies_archive_digest(tmp_path, operating_system, architecture, archive):
    intel_artifacts(tmp_path, operating_system, architecture)
    hashes = module.verify_artifacts(tmp_path, TAG, SOURCE, architecture, operating_system)
    assert hashes[archive] == hashlib.sha256(b"platform archive").hexdigest()


def test_intel_gate_rejects_wrong_architecture_provenance(tmp_path):
    intel_artifacts(tmp_path)
    module.verify_artifacts(tmp_path, TAG, SOURCE, "amd64")
    build_path = tmp_path / "BUILD-darwin-amd64.json"
    build = json.loads(build_path.read_text())
    build["architecture"] = "arm64"
    build_path.write_text(json.dumps(build))
    manifest = tmp_path / "SHA256SUMS-darwin-amd64"
    manifest.write_text("\n".join(
        f"{hashlib.sha256(build_path.read_bytes()).hexdigest()}  BUILD-darwin-amd64.json"
        if line.endswith("  BUILD-darwin-amd64.json") else line
        for line in manifest.read_text().splitlines()) + "\n")
    with pytest.raises(ValueError, match="provenance"):
        module.verify_artifacts(tmp_path, TAG, SOURCE, "amd64")


@pytest.mark.parametrize("operating_system,architecture,wheel_tag,suffix", [
    ("darwin", "amd64", "macosx_13_0_x86_64", "tar.gz"),
    ("linux", "amd64", "manylinux_2_28_x86_64", "tar.gz"),
    ("linux", "arm64", "manylinux_2_28_aarch64", "tar.gz"),
    ("windows", "amd64", "win_amd64", "zip"),
    ("windows", "arm64", "win_arm64", "zip"),
])
@pytest.mark.parametrize("fault", [None, "collision", "wrong-source", "lost-original"])
def test_append_preserves_existing_assets_and_rejects_unsafe_release(tmp_path, monkeypatch, fault, operating_system, architecture, wheel_tag, suffix):
    intel_artifacts(tmp_path, operating_system, architecture)
    original = {"id": 11, "name": "BUILD.json", "digest": "sha256:" + "b" * 64}
    release = {"id": 1, "tag_name": TAG, "draft": False, "prerelease": True,
               "assets": [original.copy()], "body": "Existing ARM64 notes", "html_url": "https://example.test/release"}
    if fault == "collision":
        release["assets"].append({"id": 12, "name": f"BUILD-{operating_system}-{architecture}.json", "digest": "sha256:" + "c" * 64})
    uploaded = []

    def github(*args, payload=None):
        endpoint = args[1]
        if "/git/ref/" in endpoint:
            return {"object": {"type": "commit", "sha": "c" * 40 if fault == "wrong-source" else SOURCE}}
        if endpoint.endswith("/tags/" + TAG) or endpoint.endswith("/releases/1"):
            if payload:
                release.update(payload)
            return release
        raise AssertionError(args)

    def upload(command, check):
        assert command[:6] == ["gh", "release", "upload", TAG, "--repo", "example/project"]
        assert "--clobber" not in command
        for filename in command[6:]:
            path = Path(filename)
            uploaded.append(path.name)
            release["assets"].append({"id": 20 + len(uploaded), "name": path.name,
                "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()})
        if fault == "lost-original":
            release["assets"] = release["assets"][1:]

    monkeypatch.setattr(module, "gh", github)
    monkeypatch.setattr(module.subprocess, "run", upload)
    if fault:
        with pytest.raises(ValueError):
            module.append_assets(tmp_path, TAG, SOURCE, "example/project", architecture, operating_system)
        if fault != "lost-original":
            assert uploaded == []
        assert release["body"] == "Existing ARM64 notes"
    else:
        module.append_assets(tmp_path, TAG, SOURCE, "example/project", architecture, operating_system)
        assert original in release["assets"]
        assert set(uploaded) == {
            f"agentsview_0.47.0+bob.2.2_{operating_system}_{architecture}.{suffix}",
            f"agentsview-0.47.0+bob.2.2-py3-none-{wheel_tag}.whl",
            f"BUILD-{operating_system}-{architecture}.json", f"SHA256SUMS-{operating_system}-{architecture}",
        }
        assert release["body"].startswith("Existing ARM64 notes\n")
        assert wheel_tag + ".whl" in release["body"]
        assert release["prerelease"] and not release["draft"]


def test_linux_gate_rejects_newer_glibc_than_wheel_claim(tmp_path):
    intel_artifacts(tmp_path, "linux")
    module.verify_artifacts(tmp_path, TAG, SOURCE, "amd64", "linux")
    build_path = tmp_path / "BUILD-linux-amd64.json"
    build = json.loads(build_path.read_text())
    build["glibc_max"] = "2.34"
    build_path.write_text(json.dumps(build))
    manifest = tmp_path / "SHA256SUMS-linux-amd64"
    manifest.write_text("\n".join(
        f"{hashlib.sha256(build_path.read_bytes()).hexdigest()}  BUILD-linux-amd64.json"
        if line.endswith("  BUILD-linux-amd64.json") else line
        for line in manifest.read_text().splitlines()) + "\n")
    with pytest.raises(ValueError, match="glibc"):
        module.verify_artifacts(tmp_path, TAG, SOURCE, "amd64", "linux")


@pytest.fixture
def full_artifacts(tmp_path):
    artifacts(tmp_path)
    for operating_system, architecture in [
        ("darwin", "amd64"), ("linux", "amd64"), ("linux", "arm64"),
        ("windows", "amd64"), ("windows", "arm64"),
    ]:
        intel_artifacts(tmp_path, operating_system, architecture)
    return tmp_path


@pytest.mark.parametrize("fault", ["missing", "stale", "mixed-source"])
def test_mirror_rejects_incomplete_or_mixed_platforms_before_network(full_artifacts, monkeypatch, fault):
    if fault == "missing":
        (full_artifacts / "SHA256SUMS-windows-arm64").unlink()
    elif fault == "stale":
        (full_artifacts / "old.zip").write_bytes(b"old")
    else:
        build_path = full_artifacts / "BUILD-windows-arm64.json"
        build = json.loads(build_path.read_text())
        build["source_sha"] = "c" * 40
        build_path.write_text(json.dumps(build))
        manifest = full_artifacts / "SHA256SUMS-windows-arm64"
        manifest.write_text("\n".join(
            f"{hashlib.sha256(build_path.read_bytes()).hexdigest()}  BUILD-windows-arm64.json"
            if line.endswith("  BUILD-windows-arm64.json") else line
            for line in manifest.read_text().splitlines()) + "\n")

    def no_network(*args, **kwargs):
        raise AssertionError("Invalid local artifacts must not reach GitHub")

    monkeypatch.setattr(module.subprocess, "run", no_network)
    with pytest.raises(ValueError, match="inventory|provenance"):
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")


@pytest.fixture
def mirror_remote(full_artifacts, monkeypatch):
    source_body = (f"Built from `{SOURCE}`. No PyPI publication.\n"
        f"https://github.com/example/project/releases/download/{TAG}/agentsview%2B.whl\n"
        "https://github.com/example/project/issues/1\n")
    source_assets = [{"id": index, "name": path.name,
        "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()}
        for index, path in enumerate(sorted(full_artifacts.iterdir()), 100)]
    source_release = {"id": 1, "tag_name": TAG, "name": "AgentsView Bob", "body": source_body,
        "draft": False, "prerelease": True, "assets": source_assets,
        "html_url": f"https://github.com/example/project/releases/tag/{TAG}"}
    state = {"source": source_release, "target": None, "uploads": [], "downloads": [],
        "writes": [], "source_sha": SOURCE, "target_sha": SOURCE,
        "target_digest": None, "corrupt_upload": False, "corrupt_download": False,
        "lose_original": False, "target_ref_checks": 0}
    monkeypatch.setenv("GH_HOST", "wrong.example.test")

    def run(command, **kwargs):
        assert command[0] == "gh", "Mirroring must not alter git branches or tags"
        if command[1:3] == ["release", "upload"]:
            assert command[:6] == ["gh", "release", "upload", TAG, "--repo", "git.example.test/example/mirror"]
            assert "--clobber" not in command
            assert kwargs["env"]["GH_HOST"] == "git.example.test"
            assert state["target"]["draft"]
            for filename in command[6:]:
                path = Path(filename)
                assert path.name not in {asset["name"] for asset in state["target"]["assets"]}
                state["uploads"].append(path.name)
                digest = state["target_digest"]
                if digest == "sha256":
                    digest = "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
                if state["corrupt_upload"]:
                    digest = "sha256:" + "0" * 64
                state["target"]["assets"].append({"id": 200 + len(state["uploads"]),
                    "name": path.name, "digest": digest})
            if state["lose_original"]:
                state["target"]["assets"].pop(0)
            return module.subprocess.CompletedProcess(command, 0)
        assert command[1] == "api"
        endpoint = command[2]
        host = command[command.index("--hostname") + 1]
        assert host in ("github.com", "git.example.test")
        slug = "example/project" if host == "github.com" else "example/mirror"
        assert endpoint.startswith(f"repos/{slug}/")
        method = command[command.index("--method") + 1] if "--method" in command else "GET"
        payload = json.loads(kwargs["input"]) if kwargs.get("input") else None
        if "/git/ref/tags/" in endpoint:
            assert method == "GET" and endpoint.endswith("/" + TAG)
            key = "source_sha" if host == "github.com" else "target_sha"
            if host != "github.com":
                state["target_ref_checks"] += 1
            value = {"object": {"type": "commit", "sha": state[key]}}
        elif endpoint.endswith("/releases/tags/" + TAG):
            assert host == "github.com" and method == "GET"
            value = state["source"]
        elif endpoint.endswith("/releases?per_page=100"):
            assert host == "git.example.test" and method == "GET"
            assert "--paginate" in command and "--slurp" in command
            value = [[state["target"]] if state["target"] else []]
        elif "/releases/assets/" in endpoint:
            assert method == "GET"
            assert command[command.index("--header") + 1] == "Accept: application/octet-stream"
            assets = state["source"]["assets"] if host == "github.com" else state["target"]["assets"]
            asset = next(asset for asset in assets if asset["id"] == int(endpoint.rsplit("/", 1)[1]))
            state["downloads"].append((host, asset["name"]))
            kwargs["stdout"].write(b"corrupt" if state["corrupt_download"] else (full_artifacts / asset["name"]).read_bytes())
            return module.subprocess.CompletedProcess(command, 0, stderr=b"")
        elif endpoint.endswith("/releases") and method == "POST":
            assert host == "git.example.test" and state["target"] is None
            assert payload["target_commitish"] == SOURCE
            state["writes"].append((method, payload.copy()))
            state["target"] = {"id": 2, **payload, "assets": [],
                "html_url": f"https://git.example.test/example/mirror/releases/tag/{TAG}"}
            value = state["target"]
        elif endpoint.endswith("/releases/2"):
            assert host == "git.example.test"
            if method == "PATCH":
                assert state["target_ref_checks"] >= 2
                assert len(state["target"]["assets"]) == 24
                state["writes"].append((method, payload.copy()))
                state["target"].update(payload)
            else:
                assert method == "GET"
            value = state["target"]
        else:
            raise AssertionError(command)
        return module.subprocess.CompletedProcess(command, 0, stdout=json.dumps(value), stderr="")

    monkeypatch.setattr(module.subprocess, "run", run)
    return state


def test_mirror_cli_verifies_enterprise_downloads_and_rewrites_only_download_urls(full_artifacts, mirror_remote, monkeypatch, capsys):
    monkeypatch.setattr("sys.argv", ["bob_release.py", "mirror", "--tag", TAG, "--source", SOURCE,
        "--directory", str(full_artifacts), "--source-repo", "example/project",
        "--target-repo", "git.example.test/example/mirror"])
    module.main()
    target = mirror_remote["target"]
    assert not target["draft"] and target["prerelease"]
    assert len(mirror_remote["uploads"]) == 24
    assert set(mirror_remote["uploads"]) == {path.name for path in full_artifacts.iterdir()}
    assert set(mirror_remote["downloads"]) == {("git.example.test", path.name) for path in full_artifacts.iterdir()}
    assert target["body"] == (f"Built from `{SOURCE}`. No PyPI publication.\n"
        f"https://git.example.test/example/mirror/releases/download/{TAG}/agentsview%2B.whl\n"
        "https://github.com/example/project/issues/1\n")
    assert mirror_remote["source"]["body"].startswith(f"Built from `{SOURCE}`.")
    assert mirror_remote["writes"][-1] == ("PATCH", {"draft": False, "prerelease": True, "make_latest": "false"})
    assert json.loads(capsys.readouterr().out)["source_sha"] == SOURCE


@pytest.mark.parametrize("fault", ["wrong-target-tag", "wrong-public-tag", "public-draft", "public-stable", "public-incomplete", "public-digest", "public-duplicate"])
def test_mirror_rejects_wrong_tag_or_unverified_public_release_without_writes(full_artifacts, mirror_remote, fault):
    if fault == "wrong-target-tag":
        mirror_remote["target_sha"] = "c" * 40
    elif fault == "wrong-public-tag":
        mirror_remote["source_sha"] = "c" * 40
    elif fault == "public-draft":
        mirror_remote["source"]["draft"] = True
    elif fault == "public-stable":
        mirror_remote["source"]["prerelease"] = False
    elif fault == "public-incomplete":
        mirror_remote["source"]["assets"].pop()
    elif fault == "public-digest":
        mirror_remote["source"]["assets"][0]["digest"] = "sha256:" + "0" * 64
    else:
        mirror_remote["source"]["assets"].append(mirror_remote["source"]["assets"][0].copy())
    with pytest.raises(ValueError):
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
    assert mirror_remote["writes"] == []
    assert mirror_remote["uploads"] == []


@pytest.mark.parametrize("fault", [None, "published", "changed-asset", "extra-asset", "changed-body", "lost-original"])
def test_mirror_resumes_only_identical_draft_assets_without_overwrite(full_artifacts, mirror_remote, fault):
    original = mirror_remote["source"]["assets"][0].copy()
    mirror_remote["target"] = {"id": 2, "tag_name": TAG, "name": "AgentsView Bob", "draft": True,
        "prerelease": True, "assets": [original.copy()],
        "body": mirror_remote["source"]["body"].replace("https://github.com/example/project/releases/download/", "https://git.example.test/example/mirror/releases/download/"),
        "html_url": f"https://git.example.test/example/mirror/releases/tag/{TAG}"}
    mirror_remote["target_digest"] = "sha256"
    if fault == "published":
        mirror_remote["target"]["draft"] = False
    elif fault == "changed-asset":
        mirror_remote["target"]["assets"][0]["digest"] = "sha256:" + "0" * 64
    elif fault == "extra-asset":
        mirror_remote["target"]["assets"].append({"id": 9, "name": "old.zip", "digest": None})
    elif fault == "changed-body":
        mirror_remote["target"]["body"] = "Different release"
    elif fault == "lost-original":
        mirror_remote["lose_original"] = True
    if fault:
        with pytest.raises(ValueError):
            module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
        assert mirror_remote["target"]["draft"] == (fault != "published")
        assert mirror_remote["writes"] == []
        if fault != "lost-original":
            assert mirror_remote["uploads"] == []
    else:
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
        assert original in mirror_remote["target"]["assets"]
        assert len(mirror_remote["uploads"]) == 23
        assert original["name"] not in mirror_remote["uploads"]
        assert not mirror_remote["target"]["draft"]
        assert mirror_remote["downloads"] == []


@pytest.mark.parametrize("fault", ["upload-digest", "download-bytes"])
def test_mirror_leaves_corrupt_uploads_unpublished(full_artifacts, mirror_remote, fault):
    mirror_remote["corrupt_upload" if fault == "upload-digest" else "corrupt_download"] = True
    with pytest.raises(ValueError, match="Checksum"):
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
    assert mirror_remote["target"]["draft"]
    assert [method for method, _ in mirror_remote["writes"]] == ["POST"]


def test_mirror_verifies_public_assets_by_download_when_digest_unavailable(full_artifacts, mirror_remote):
    mirror_remote["source"]["assets"][0]["digest"] = None
    mirror_remote["target_digest"] = "sha256"
    module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
    assert mirror_remote["downloads"] == [("github.com", "BUILD-darwin-amd64.json")]
    assert not mirror_remote["target"]["draft"]


def test_mirror_rejects_corrupt_public_download_before_creating_target(full_artifacts, mirror_remote):
    mirror_remote["source"]["assets"][0]["digest"] = None
    mirror_remote["corrupt_download"] = True
    with pytest.raises(ValueError, match="Checksum"):
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
    assert mirror_remote["target"] is None
    assert mirror_remote["writes"] == []
    assert mirror_remote["uploads"] == []


@pytest.mark.parametrize("field,value", [("prerelease", False), ("name", "Different release")])
def test_mirror_refuses_mismatched_target_draft_metadata(full_artifacts, mirror_remote, field, value):
    mirror_remote["target"] = {"id": 2, "tag_name": TAG, "name": "AgentsView Bob", "draft": True,
        "prerelease": True, "assets": [],
        "body": mirror_remote["source"]["body"].replace("https://github.com/example/project/releases/download/", "https://git.example.test/example/mirror/releases/download/"),
        "html_url": f"https://git.example.test/example/mirror/releases/tag/{TAG}"}
    mirror_remote["target"][field] = value
    with pytest.raises(ValueError):
        module.mirror(full_artifacts, TAG, SOURCE, "example/project", "git.example.test/example/mirror")
    assert mirror_remote["writes"] == []
    assert mirror_remote["uploads"] == []
