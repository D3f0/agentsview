"""Validate, publish, and mirror native Bob artifacts without replacing assets."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


def release_parameters(tag: str, source: str) -> dict[str, str]:
    match = re.fullmatch(r"v(\d+\.\d+\.\d+)-bob\.(\d+)\.(\d+)", tag)
    if not match or tag == "v0.47.0-bob.2.1":
        raise ValueError("Use a new Bob prerelease tag; the existing 2.1 release is immutable")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("source_sha must identify one exact 40-character commit")
    package = f"{match[1]}+bob.{match[2]}.{match[3]}"
    return {"tag": tag, "source": source, "package": package}


def platform_parameters(architecture: str, operating_system: str) -> dict[str, str]:
    if architecture not in ("arm64", "amd64") or operating_system not in ("darwin", "linux", "windows"):
        raise ValueError("Unsupported release platform")
    wheel_arch = "x86_64" if architecture == "amd64" else "arm64"
    if operating_system == "linux":
        wheel_tag = "manylinux_2_28_" + ("x86_64" if architecture == "amd64" else "aarch64")
    elif operating_system == "windows":
        wheel_tag = "win_" + architecture
    else:
        wheel_tag = f"macosx_13_0_{wheel_arch}"
    original = operating_system == "darwin" and architecture == "arm64"
    return {
        "wheel_tag": wheel_tag,
        "build_name": "BUILD.json" if original else f"BUILD-{operating_system}-{architecture}.json",
        "manifest": "SHA256SUMS" if original else f"SHA256SUMS-{operating_system}-{architecture}",
        "archive_suffix": "zip" if operating_system == "windows" else "tar.gz",
    }


def verify_artifacts(directory: Path, tag: str, source: str, architecture: str = "arm64", operating_system: str = "darwin") -> dict[str, str]:
    params = release_parameters(tag, source)
    platform = platform_parameters(architecture, operating_system)
    wheel_tag = platform["wheel_tag"]
    build_name = platform["build_name"]
    manifest = platform["manifest"]
    names = {
        f"agentsview_{params['package']}_{operating_system}_{architecture}.{platform['archive_suffix']}",
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
    elif operating_system == "linux":
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


def append_assets(directory: Path, tag: str, source: str, repo: str, architecture: str = "amd64", operating_system: str = "darwin") -> None:
    hashes = verify_artifacts(directory, tag, source, architecture, operating_system)
    params = release_parameters(tag, source)
    ref = gh("api", f"repos/{repo}/git/ref/tags/{tag}")
    if ref["object"]["type"] != "commit" or ref["object"]["sha"] != source:
        raise ValueError("Release tag does not identify the verified source commit")
    release = gh("api", f"repos/{repo}/releases/tags/{tag}")
    if release["tag_name"] != tag or release["draft"] or not release["prerelease"]:
        raise ValueError("Additional assets require an existing published prerelease")
    original = {asset["name"]: (asset["id"], asset.get("digest")) for asset in release["assets"]}
    if original.keys() & hashes.keys():
        raise ValueError("Platform release assets already exist; refusing to overwrite")
    subprocess.run(["gh", "release", "upload", tag, "--repo", repo,
                    *[str(directory / name) for name in sorted(hashes)]], check=True)
    uploaded = gh("api", f"repos/{repo}/releases/{release['id']}")
    assets = {asset["name"]: asset for asset in uploaded["assets"]}
    if (uploaded["tag_name"] != tag or uploaded["draft"] or not uploaded["prerelease"]
            or set(assets) != original.keys() | hashes.keys()
            or any((assets[name]["id"], assets[name].get("digest")) != value for name, value in original.items())
            or any(assets[name].get("digest") != "sha256:" + digest for name, digest in hashes.items())):
        raise ValueError("Platform upload failed checksum verification or changed existing assets")
    platform = platform_parameters(architecture, operating_system)
    wheel_tag = platform["wheel_tag"]
    wheel = f"agentsview-{params['package']}-py3-none-{wheel_tag}.whl"
    wheel_url = f"https://github.com/{repo}/releases/download/{tag}/{wheel.replace('+', '%2B')}"
    platform_notes = {
        "linux": f"Linux {architecture}, glibc 2.28+, is also available, built from the same source.",
        "darwin": f"macOS {architecture}, macOS 13+, is also available, built from the same source. Ad-hoc signed, not Developer ID notarized.",
        "windows": f"Windows {architecture} is also available, built from the same source. The executable is not Authenticode signed.",
    }[operating_system]
    body = ((uploaded.get("body") or "").rstrip() + f"\n\n{platform_notes} "
            f"Checksums: `{platform['manifest']}`; provenance: `{platform['build_name']}`.\n\n"
            f"```bash\nuvx --from '{wheel_url}' agentsview --version\n```\n")
    published = gh("api", f"repos/{repo}/releases/{release['id']}", "--method", "PATCH", payload={"body": body})
    if published["body"] != body or published["draft"] or not published["prerelease"]:
        raise ValueError("Platform release notes were not published safely")
    print(json.dumps({"release_url": published["html_url"], "wheel_url": wheel_url, "source_sha": source}, indent=2))


def repository_location(repo: str) -> tuple[str, str]:
    match = re.fullmatch(r"(?:(?P<host>[A-Za-z0-9.-]+)/)?(?P<owner>[A-Za-z0-9_.-]+)/(?P<repo>[A-Za-z0-9_.-]+)", repo)
    if not match:
        raise ValueError("Repository must be OWNER/REPO or HOST/OWNER/REPO")
    return match["host"] or "github.com", f"{match['owner']}/{match['repo']}"


def verify_full_artifacts(directory: Path, tag: str, source: str) -> dict[str, str]:
    params = release_parameters(tag, source)
    hashes: dict[str, str] = {}
    with tempfile.TemporaryDirectory(prefix="bob-release-verify-") as temporary:
        for operating_system in ("darwin", "linux", "windows"):
            for architecture in ("arm64", "amd64"):
                platform = platform_parameters(architecture, operating_system)
                names = {
                    f"agentsview_{params['package']}_{operating_system}_{architecture}.{platform['archive_suffix']}",
                    f"agentsview-{params['package']}-py3-none-{platform['wheel_tag']}.whl",
                    platform["build_name"], platform["manifest"],
                }
                selected = Path(temporary) / f"{operating_system}-{architecture}"
                selected.mkdir()
                for name in names:
                    artifact = directory / name
                    if not artifact.is_file():
                        raise ValueError("Six-platform artifact inventory is missing files")
                    (selected / name).symlink_to(artifact.resolve())
                hashes.update(verify_artifacts(selected, tag, source, architecture, operating_system))
    if {path.name for path in directory.iterdir()} != set(hashes) or len(hashes) != 24:
        raise ValueError("Six-platform artifact inventory contains stale or unexpected files")
    return hashes


def verify_remote_assets(assets: list[dict], hashes: dict[str, str], host: str, repo: str,
                         *, complete: bool = True) -> dict[str, int]:
    names = [asset["name"] for asset in assets]
    if (len(names) != len(set(names)) or not set(names) <= set(hashes)
            or (complete and set(names) != set(hashes))):
        raise ValueError("Remote release asset inventory differs from verified local artifacts")
    verified: dict[str, int] = {}
    for asset in assets:
        name = asset["name"]
        digest = asset.get("digest")
        if digest is not None:
            if digest != "sha256:" + hashes[name]:
                raise ValueError(f"Checksum mismatch for remote asset: {name}")
        else:
            with tempfile.TemporaryFile() as downloaded:
                result = subprocess.run([
                    "gh", "api", f"repos/{repo}/releases/assets/{asset['id']}",
                    "--hostname", host, "--header", "Accept: application/octet-stream",
                ], stdout=downloaded, stderr=subprocess.PIPE)
                if result.returncode:
                    raise RuntimeError(f"GitHub asset download failed: {result.stderr.decode().strip()}")
                downloaded.seek(0)
                actual = hashlib.sha256()
                while chunk := downloaded.read(1024 * 1024):
                    actual.update(chunk)
                if actual.hexdigest() != hashes[name]:
                    raise ValueError(f"Checksum mismatch for downloaded remote asset: {name}")
        verified[name] = asset["id"]
    return verified


def mirror(directory: Path, tag: str, source: str, source_repo: str, target_repo: str) -> None:
    """Mirror a complete public prerelease; the exact target tag must already exist."""
    hashes = verify_full_artifacts(directory, tag, source)
    source_host, source_slug = repository_location(source_repo)
    target_host, target_slug = repository_location(target_repo)
    if source_host != "github.com" or (source_host, source_slug) == (target_host, target_slug):
        raise ValueError("Mirror requires a public GitHub source and a different target repository")

    def check_tag(host: str, repo: str) -> None:
        ref = gh("api", f"repos/{repo}/git/ref/tags/{tag}", "--hostname", host)
        if ref["object"]["type"] != "commit" or ref["object"]["sha"] != source:
            raise ValueError("Release tag does not identify the verified source commit; push the exact source tag first")

    check_tag(source_host, source_slug)
    public = gh("api", f"repos/{source_slug}/releases/tags/{tag}", "--hostname", source_host)
    if public["tag_name"] != tag or public["draft"] or not public["prerelease"]:
        raise ValueError("Mirror source must be the existing published Bob prerelease")
    verify_remote_assets(public["assets"], hashes, source_host, source_slug)
    check_tag(target_host, target_slug)
    body = (public.get("body") or "").replace(
        f"https://{source_host}/{source_slug}/releases/download/",
        f"https://{target_host}/{target_slug}/releases/download/",
    )
    pages = gh("api", f"repos/{target_slug}/releases?per_page=100", "--hostname", target_host,
               "--paginate", "--slurp")
    releases = [release for page in pages for release in page if release["tag_name"] == tag]
    if len(releases) > 1:
        raise ValueError("Target has multiple releases for the same tag")
    if releases:
        release = releases[0]
        if (not release["draft"] or not release["prerelease"]
                or release.get("body", "") != body or release["name"] != public["name"]):
            raise ValueError("Target release already exists; only an identical draft can resume")
        original = verify_remote_assets(release["assets"], hashes, target_host, target_slug, complete=False)
    else:
        release = gh("api", f"repos/{target_slug}/releases", "--hostname", target_host, "--method", "POST",
                     payload={"tag_name": tag, "target_commitish": source, "name": public["name"],
                              "body": body, "draft": True, "prerelease": True})
        if (release["tag_name"] != tag or not release["draft"] or not release["prerelease"]
                or release["assets"] or release.get("body") != body or release["name"] != public["name"]):
            raise ValueError("New target draft release state is not safe for upload")
        original = {}
    missing = sorted(hashes.keys() - original.keys())
    if missing:
        subprocess.run(["gh", "release", "upload", tag, "--repo", f"{target_host}/{target_slug}",
                        *[str(directory / name) for name in missing]], check=True,
                       env={**os.environ, "GH_HOST": target_host})
    uploaded = gh("api", f"repos/{target_slug}/releases/{release['id']}", "--hostname", target_host)
    if (uploaded["tag_name"] != tag or not uploaded["draft"] or not uploaded["prerelease"]
            or uploaded.get("body") != body or uploaded["name"] != public["name"]):
        raise ValueError("Target draft metadata changed before publication")
    verified = verify_remote_assets(uploaded["assets"], hashes, target_host, target_slug)
    if any(verified.get(name) != asset_id for name, asset_id in original.items()):
        raise ValueError("Target upload changed existing assets")
    check_tag(target_host, target_slug)
    published = gh("api", f"repos/{target_slug}/releases/{release['id']}", "--hostname", target_host,
                   "--method", "PATCH", payload={"draft": False, "prerelease": True, "make_latest": "false"})
    if (published["tag_name"] != tag or published["draft"] or not published["prerelease"]
            or published.get("body") != body or published["name"] != public["name"]
            or sorted((asset["name"], asset["id"], asset.get("digest")) for asset in published["assets"])
            != sorted((asset["name"], asset["id"], asset.get("digest")) for asset in uploaded["assets"])):
        raise ValueError("Target release was not published safely as an identical prerelease")
    print(json.dumps({"release_url": published["html_url"], "source_sha": source, "asset_count": len(hashes)}, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["prepare", "verify", "publish", "append", "mirror"])
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--directory", type=Path, default=Path("artifacts"))
    parser.add_argument("--architecture", choices=["arm64", "amd64"], default="arm64")
    parser.add_argument("--os", choices=["darwin", "linux", "windows"], default="darwin")
    parser.add_argument("--source-repo", help="Public mirror source as OWNER/REPO")
    parser.add_argument("--target-repo", help="Mirror destination as HOST/OWNER/REPO; exact source tag must already exist")
    args = parser.parse_args()
    params = release_parameters(args.tag, args.source)
    if args.command == "prepare":
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key, value in params.items():
                output.write(f"{key}={value}\n")
    elif args.command == "verify":
        print(json.dumps(verify_artifacts(args.directory, args.tag, args.source, args.architecture, args.os), indent=2))
    elif args.command == "append":
        append_assets(args.directory, args.tag, args.source, os.environ["GH_REPO"], args.architecture, args.os)
    elif args.command == "mirror":
        if not args.source_repo or not args.target_repo:
            parser.error("mirror requires --source-repo and --target-repo")
        mirror(args.directory, args.tag, args.source, args.source_repo, args.target_repo)
    else:
        if args.architecture != "arm64" or args.os != "darwin":
            raise ValueError("Use append to extend an existing prerelease")
        publish(args.directory, args.tag, args.source, os.environ["GH_REPO"])


if __name__ == "__main__":
    main()
