#!/usr/bin/env python3
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0
"""Assemble the main documentation and GA release snapshots in one Pages artifact."""

from pathlib import Path
import re
import subprocess
from tempfile import TemporaryDirectory

from mkdocs.commands.build import build
from mkdocs.config import load_config


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def build_version(config_path, output, version, links, url):
    config = load_config(config_file=str(config_path), site_dir=str(output), strict=True)
    config.site_url = url
    if version != "main":
        config.edit_uri = f"blob/{version}/docs/"
    config.nav.append({f"Documentation version ({version})": links})
    build(config)


def main():
    output = Path("site").resolve()
    base = load_config().site_url.rstrip("/") + "/"
    tags = [tag for tag in git("tag", "--sort=-version:refname").splitlines()
            if re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag)]
    snapshots = []
    links = [{"main (development)": base}]
    for tag in tags:
        if git("ls-tree", "--name-only", tag, "--", "mkdocs.yml"):
            snapshots.append(tag)
            links.append({tag: f"{base}{tag}/"})
        else:
            links.append({f"{tag} (GitHub)": f"https://github.com/NVIDIA/k8s-launch-kit/tree/{tag}/docs"})

    build_version(Path("mkdocs.yml"), output, "main", links, base)
    with TemporaryDirectory() as directory:
        checkout = Path(directory) / "release"
        for tag in snapshots:
            git("worktree", "add", "--quiet", "--detach", str(checkout), tag)
            try:
                build_version(checkout / "mkdocs.yml", output / tag, tag, links, f"{base}{tag}/")
            finally:
                git("worktree", "remove", "--force", str(checkout))


if __name__ == "__main__":
    main()
