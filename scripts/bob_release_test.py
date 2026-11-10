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


def intel_artifacts(path):
    data = {
        "agentsview_0.47.0+bob.2.2_darwin_amd64.tar.gz": b"intel archive",
        "agentsview-0.47.0+bob.2.2-py3-none-macosx_13_0_x86_64.whl": b"intel wheel",
        "BUILD-darwin-amd64.json": json.dumps({
            "tag": TAG, "source_sha": SOURCE, "package_version": "0.47.0+bob.2.2",
            "api_version": 15, "data_version": 169, "minos": "13.0",
            "architecture": "amd64", "workflow_sha": "a" * 40,
        }).encode(),
    }
    for name, value in data.items():
        (path / name).write_bytes(value)
    (path / "SHA256SUMS-darwin-amd64").write_text("".join(
        f"{hashlib.sha256(value).hexdigest()}  {name}\n" for name, value in data.items()))


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


@pytest.mark.parametrize("fault", [None, "collision", "wrong-source", "lost-original"])
def test_append_preserves_existing_assets_and_rejects_unsafe_release(tmp_path, monkeypatch, fault):
    intel_artifacts(tmp_path)
    original = {"id": 11, "name": "BUILD.json", "digest": "sha256:" + "b" * 64}
    release = {"id": 1, "tag_name": TAG, "draft": False, "prerelease": True,
               "assets": [original.copy()], "body": "Existing ARM64 notes", "html_url": "https://example.test/release"}
    if fault == "collision":
        release["assets"].append({"id": 12, "name": "BUILD-darwin-amd64.json", "digest": "sha256:" + "c" * 64})
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
            module.append_intel(tmp_path, TAG, SOURCE, "example/project")
        if fault != "lost-original":
            assert uploaded == []
        assert release["body"] == "Existing ARM64 notes"
    else:
        module.append_intel(tmp_path, TAG, SOURCE, "example/project")
        assert original in release["assets"]
        assert set(uploaded) == {
            "agentsview_0.47.0+bob.2.2_darwin_amd64.tar.gz",
            "agentsview-0.47.0+bob.2.2-py3-none-macosx_13_0_x86_64.whl",
            "BUILD-darwin-amd64.json", "SHA256SUMS-darwin-amd64",
        }
        assert release["body"].startswith("Existing ARM64 notes\n")
        assert "macosx_13_0_x86_64.whl" in release["body"]
        assert release["prerelease"] and not release["draft"]
