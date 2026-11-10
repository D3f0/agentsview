"""Validate and publish native Bob artifacts through the fork workflow only."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess


def release_parameters(tag: str, source: str) -> dict[str, str]:
    match = re.fullmatch(r"v(\d+\.\d+\.\d+)-bob\.(\d+)\.(\d+)", tag)
    if not match or tag == "v0.47.0-bob.2.1":
        raise ValueError("Use a new Bob prerelease tag; the existing 2.1 release is immutable")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("source_sha must identify one exact 40-character commit")
    package = f"{match[1]}+bob.{match[2]}.{match[3]}"
    return {"tag": tag, "source": source, "package": package}


def verify_artifacts(directory: Path, tag: str, source: str) -> dict[str, str]:
    params = release_parameters(tag, source)
    names = {
        f"agentsview_{params['package']}_darwin_arm64.tar.gz",
        f"agentsview-{params['package']}-py3-none-macosx_13_0_arm64.whl",
        "BUILD.json",
    }
    if {p.name for p in directory.iterdir()} != names | {"SHA256SUMS"}:
        raise ValueError("Artifact inventory is missing files or contains stale files")
    hashes: dict[str, str] = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        digest, name = line.split("  ", 1)
        if name not in names or name in hashes or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("Invalid or duplicate checksum entry")
        actual = hashlib.sha256((directory / name).read_bytes()).hexdigest()
        if actual != digest:
            raise ValueError(f"Checksum mismatch: {name}")
        hashes[name] = digest
    if set(hashes) != names:
        raise ValueError("Checksum manifest does not cover the artifact inventory")
    build = json.loads((directory / "BUILD.json").read_text())
    expected = {"tag": tag, "source_sha": source, "package_version": params["package"],
                "api_version": 15, "data_version": 169, "minos": "13.0", "architecture": "arm64"}
    if any(build.get(key) != value for key, value in expected.items()):
        raise ValueError("Build provenance does not match the requested source and contracts")
    hashes["SHA256SUMS"] = hashlib.sha256((directory / "SHA256SUMS").read_bytes()).hexdigest()
    return hashes


def gh(*args: str, payload: dict | None = None) -> object:
    command = ["gh", *args]
    if payload is not None:
        command += ["--input", "-"]
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            text=True, capture_output=True, check=True)
    return json.loads(result.stdout) if result.stdout.strip() else None


def publish(directory: Path, tag: str, source: str, repo: str) -> None:
    hashes = verify_artifacts(directory, tag, source)
    params = release_parameters(tag, source)
    ref = gh("api", f"repos/{repo}/git/ref/tags/{tag}")
    if ref["object"]["type"] != "commit" or ref["object"]["sha"] != source:
        raise ValueError("Release tag does not identify the verified source commit")
    releases = gh("api", f"repos/{repo}/releases?per_page=100")
    if any(release["tag_name"] == tag for release in releases):
        raise ValueError("Release already exists; refusing to overwrite its assets")
    wheel = f"agentsview-{params['package']}-py3-none-macosx_13_0_arm64.whl"
    wheel_url = f"https://github.com/{repo}/releases/download/{tag}/{wheel.replace('+', '%2B')}"
    body = (f"Bob 1.x CLI and IDE history plus Bob 2.x SQLite support, built from `{source}`. "
            "Preserves streamed reasoning, response text, tool results, and archived history. "
            "Bob-only archive upgrade 168 to 169; server API 15 and PostgreSQL replica commands remain supported.\n\n"
            "macOS ARM64, macOS 13+. Ad-hoc signed; not Developer ID notarized. "
            "No global PyPI publication.\n\n"
            f"```bash\nuvx --from '{wheel_url}' agentsview --version\n```\n")
    release = gh("api", f"repos/{repo}/releases", "--method", "POST", payload={
        "tag_name": tag, "target_commitish": source, "name": f"AgentsView {tag} — Bob compatibility",
        "body": body, "draft": True, "prerelease": True})
    if not release["draft"] or release["tag_name"] != tag or release["assets"]:
        raise ValueError("New draft release state is not safe for upload")
    subprocess.run(["gh", "release", "upload", tag, "--repo", repo,
                    *[str(directory / name) for name in sorted(hashes)]], check=True)
    uploaded = gh("api", f"repos/{repo}/releases/{release['id']}")
    actual = {asset["name"]: asset.get("digest") for asset in uploaded["assets"]}
    expected = {name: "sha256:" + digest for name, digest in hashes.items()}
    if actual != expected or not uploaded["draft"] or uploaded["tag_name"] != tag:
        raise ValueError("Uploaded draft assets failed exact checksum/inventory verification")
    published = gh("api", f"repos/{repo}/releases/{release['id']}", "--method", "PATCH",
                   payload={"draft": False, "prerelease": True, "make_latest": "false"})
    if published["draft"] or not published["prerelease"]:
        raise ValueError("Release was not published as a prerelease")
    print(json.dumps({"release_url": published["html_url"], "wheel_url": wheel_url,
                      "tag_sha": source, "build_sha": json.loads((directory / "BUILD.json").read_text())["workflow_sha"]}, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["prepare", "verify", "publish"])
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--directory", type=Path, default=Path("artifacts"))
    args = parser.parse_args()
    params = release_parameters(args.tag, args.source)
    if args.command == "prepare":
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key, value in params.items():
                output.write(f"{key}={value}\n")
    elif args.command == "verify":
        print(json.dumps(verify_artifacts(args.directory, args.tag, args.source), indent=2))
    else:
        publish(args.directory, args.tag, args.source, os.environ["GH_REPO"])


if __name__ == "__main__":
    main()
