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
