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


def verify_artifacts(directory: Path, tag: str, source: str, architecture: str = "arm64", operating_system: str = "darwin") -> dict[str, str]:
    params = release_parameters(tag, source)
    if architecture not in ("arm64", "amd64") or operating_system not in ("darwin", "linux") or (operating_system == "linux" and architecture != "amd64"):
        raise ValueError("Unsupported release platform")
    wheel_arch = "x86_64" if architecture == "amd64" else "arm64"
    wheel_tag = "manylinux_2_28_x86_64" if operating_system == "linux" else f"macosx_13_0_{wheel_arch}"
    build_name = f"BUILD-{operating_system}-amd64.json" if architecture == "amd64" else "BUILD.json"
    manifest = f"SHA256SUMS-{operating_system}-amd64" if architecture == "amd64" else "SHA256SUMS"
    names = {
        f"agentsview_{params['package']}_{operating_system}_{architecture}.tar.gz",
        f"agentsview-{params['package']}-py3-none-{wheel_tag}.whl",
        build_name,
    }
    if {p.name for p in directory.iterdir()} != names | {manifest}:
        raise ValueError("Artifact inventory is missing files or contains stale files")
    hashes: dict[str, str] = {}
    for line in (directory / manifest).read_text().splitlines():
        digest, name = line.split("  ", 1)
        if name not in names or name in hashes or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("Invalid or duplicate checksum entry")
        actual = hashlib.sha256((directory / name).read_bytes()).hexdigest()
        if actual != digest:
            raise ValueError(f"Checksum mismatch: {name}")
        hashes[name] = digest
    if set(hashes) != names:
        raise ValueError("Checksum manifest does not cover the artifact inventory")
    build = json.loads((directory / build_name).read_text())
    expected = {"tag": tag, "source_sha": source, "package_version": params["package"],
                "api_version": 15, "data_version": 169, "architecture": architecture}
    if operating_system == "darwin":
        expected["minos"] = "13.0"
    else:
        glibc = build.get("glibc_max", "")
        if not re.fullmatch(r"\d+\.\d+", glibc) or not (2, 0) <= tuple(map(int, glibc.split("."))) <= (2, 28):
            raise ValueError("Build glibc requirement exceeds the manylinux 2.28 wheel contract")
    if any(build.get(key) != value for key, value in expected.items()):
        raise ValueError("Build provenance does not match the requested source and contracts")
    hashes[manifest] = hashlib.sha256((directory / manifest).read_bytes()).hexdigest()
    return hashes


def gh(*args: str, payload: dict | None = None) -> object:
    command = ["gh", *args]
    if payload is not None:
        command += ["--input", "-"]
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"GitHub API request failed: {result.stderr.strip()} {result.stdout.strip()}")
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
        "tag_name": tag, "name": f"AgentsView {tag} — Bob compatibility",
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


def append_intel(directory: Path, tag: str, source: str, repo: str, operating_system: str = "darwin") -> None:
    hashes = verify_artifacts(directory, tag, source, "amd64", operating_system)
    params = release_parameters(tag, source)
    ref = gh("api", f"repos/{repo}/git/ref/tags/{tag}")
    if ref["object"]["type"] != "commit" or ref["object"]["sha"] != source:
        raise ValueError("Release tag does not identify the verified source commit")
    release = gh("api", f"repos/{repo}/releases/tags/{tag}")
    if release["tag_name"] != tag or release["draft"] or not release["prerelease"]:
        raise ValueError("Intel assets require an existing published prerelease")
    original = {asset["name"]: (asset["id"], asset.get("digest")) for asset in release["assets"]}
    if original.keys() & hashes.keys():
        raise ValueError("Intel release assets already exist; refusing to overwrite")
    subprocess.run(["gh", "release", "upload", tag, "--repo", repo,
                    *[str(directory / name) for name in sorted(hashes)]], check=True)
    uploaded = gh("api", f"repos/{repo}/releases/{release['id']}")
    assets = {asset["name"]: asset for asset in uploaded["assets"]}
    if (uploaded["tag_name"] != tag or uploaded["draft"] or not uploaded["prerelease"]
            or set(assets) != original.keys() | hashes.keys()
            or any((assets[name]["id"], assets[name].get("digest")) != value for name, value in original.items())
            or any(assets[name].get("digest") != "sha256:" + digest for name, digest in hashes.items())):
        raise ValueError("Intel upload failed checksum verification or changed existing assets")
    wheel_tag = "manylinux_2_28_x86_64" if operating_system == "linux" else "macosx_13_0_x86_64"
    wheel = f"agentsview-{params['package']}-py3-none-{wheel_tag}.whl"
    wheel_url = f"https://github.com/{repo}/releases/download/{tag}/{wheel.replace('+', '%2B')}"
    platform_notes = ("Linux x86_64 (Intel/AMD), glibc 2.28+, is also available, built from the same source."
                      if operating_system == "linux" else
                      "Intel macOS (x86_64) is also available, built from the same source. "
                      "macOS 13+; ad-hoc signed, not Developer ID notarized.")
    body = ((uploaded.get("body") or "").rstrip() + f"\n\n{platform_notes} "
            f"Checksums: `SHA256SUMS-{operating_system}-amd64`; provenance: `BUILD-{operating_system}-amd64.json`.\n\n"
            f"```bash\nuvx --from '{wheel_url}' agentsview --version\n```\n")
    published = gh("api", f"repos/{repo}/releases/{release['id']}", "--method", "PATCH", payload={"body": body})
    if published["body"] != body or published["draft"] or not published["prerelease"]:
        raise ValueError("Intel release notes were not published safely")
    print(json.dumps({"release_url": published["html_url"], "wheel_url": wheel_url, "source_sha": source}, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["prepare", "verify", "publish", "append-intel"])
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--directory", type=Path, default=Path("artifacts"))
    parser.add_argument("--architecture", choices=["arm64", "amd64"], default="arm64")
    parser.add_argument("--os", choices=["darwin", "linux"], default="darwin")
    args = parser.parse_args()
    params = release_parameters(args.tag, args.source)
    if args.command == "prepare":
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key, value in params.items():
                output.write(f"{key}={value}\n")
    elif args.command == "verify":
        print(json.dumps(verify_artifacts(args.directory, args.tag, args.source, args.architecture, args.os), indent=2))
    elif args.command == "append-intel":
        append_intel(args.directory, args.tag, args.source, os.environ["GH_REPO"], args.os)
    else:
        if args.architecture != "arm64" or args.os != "darwin":
            raise ValueError("Use append-intel to extend an existing prerelease")
        publish(args.directory, args.tag, args.source, os.environ["GH_REPO"])


if __name__ == "__main__":
    main()
