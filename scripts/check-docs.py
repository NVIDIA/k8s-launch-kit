#!/usr/bin/env python3
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0
"""Check documented CLI syntax, flag scopes, navigation, and offline examples.

Only --help and explicitly constructed offline generate
commands are executed. Documentation shell text is parsed, never executed.
Architecture plans describe future interfaces and are excluded from syntax checks.
"""

import argparse
import json
from pathlib import Path
import re
import shlex
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
COMMANDS = ("", "discover", "generate", "deploy", "validate", "clean",
            "preset list", "preset update", "sosreport", "schema", "version")


def run(binary, *args):
    result = subprocess.run([str(binary), *args], cwd=ROOT, text=True,
                            capture_output=True, timeout=90, check=False)
    if result.returncode:
        raise ValueError(f"l8k {' '.join(args)} failed:\n{result.stderr}\n{result.stdout}")
    return result.stdout


def shell_blocks(text):
    """Yield executable examples, including indented tabs and RST code blocks."""
    lines = text.splitlines()
    inside = False
    rst_indent = None
    block = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(".. code-block::"):
            if block:
                yield "\n".join(block)
                block = []
            rst_indent = len(line) - len(line.lstrip()) if stripped.endswith(("bash", "sh", "shell")) else None
            continue
        if rst_indent is not None:
            if not stripped or len(line) - len(line.lstrip()) > rst_indent:
                block.append(line)
                continue
            yield "\n".join(block)
            block = []
            rst_indent = None
        if stripped.startswith("```"):
            if inside:
                yield "\n".join(block)
                block = []
            inside = stripped in ("```bash", "```sh", "```shell")
        elif inside:
            block.append(line)
    if block:
        yield "\n".join(block)


def examples(text):
    for block in shell_blocks(text):
        logical = re.sub(r"\\\n\s*", " ", block)
        for line in logical.splitlines():
            if line.lstrip().startswith("#"):
                continue
            match = re.search(r"(?:^|\$\()\s*(?:\./build/)?l8k\s+(.+)", line)
            if not match:
                continue
            # Shell placeholders/optional notation are documentation, not syntax.
            command = re.split(r"\s+(?:\||>|2>|;|&&)", match[1], maxsplit=1)[0]
            command = re.sub(r"\[(--[^\]]+)\]", r"\1", command)
            tokens = shlex.split(command.rstrip(")"), comments=True)
            name = ""
            if tokens and not tokens[0].startswith("-"):
                name = tokens.pop(0)
                if name == "preset" and tokens:
                    name += " " + tokens.pop(0)
            yield name, tokens


def check_syntax(binary):
    flags = {}
    help_texts = []
    for command in COMMANDS:
        help_text = run(binary, *command.split(), "--help")
        if "Examples:" in help_text:
            example_text = help_text.split("Examples:", 1)[1]
            example_text = re.split(r"\n[A-Z][^\n]*:\n", example_text, maxsplit=1)[0]
            help_texts.append((f"l8k {command} --help", example_text))
        flags[command] = dict(re.findall(
            r"^\s+(?:-\w, )?--([\w-]+)(?:\s+(string|strings|int|duration|float64))?",
            help_text, re.MULTILINE))
    paths = [ROOT / "README.md", *sorted((ROOT / "docs").rglob("*.md")),
             *sorted((ROOT / "docs").glob("*.rst")),
             *sorted((ROOT / "skills").rglob("*.md"))]
    sources = [(str(p.relative_to(ROOT)), p.read_text()) for p in paths
               if not p.name.endswith("-plan.md")]
    # Cobra examples are plain text; wrap them to use the same syntax parser.
    sources += [(name, "```bash\n" + text + "\n```") for name, text in help_texts]
    count = 0
    for source, text in sources:
        for command, tokens in examples(text):
            if command not in flags:
                raise ValueError(f"{source}: unknown command {command!r}")
            count += 1
            for index, token in enumerate(tokens):
                if not token.startswith("--"):
                    continue
                flag = token[2:].split("=", 1)[0]
                if flag not in flags[command]:
                    raise ValueError(f"{source}: --{flag} is not accepted by l8k {command}")
                if flags[command][flag] and "=" not in token:
                    if index + 1 == len(tokens) or tokens[index + 1].startswith("--"):
                        raise ValueError(f"{source}: --{flag} requires a value")
    reference = (ROOT / "docs/reference/cli.md").read_text()
    matrix = reference.split("| Flags | Root |", 1)[1].split("\n\n", 1)[0]
    for line in matrix.splitlines()[2:]:
        cells = [cell.strip() for cell in line.strip("|").split("|")]
        for flag in re.findall(r"`--([\w-]+)`", cells[0]):
            for command, cell in zip(COMMANDS[:6], cells[1:]):
                accepted = flag in flags[command]
                if accepted != cell.startswith("yes"):
                    raise ValueError(f"CLI matrix: --{flag}, {command or 'root'} scope is incorrect")
    print(f"CLI syntax: {count} documentation/help examples and lifecycle flag matrix passed")


def check_index():
    nav = (ROOT / "mkdocs.yml").read_text().split("\nnav:\n", 1)[1]
    paths = re.findall(r"^\s*- [^\n]+: ([\w./-]+\.md)$", nav, re.MULTILINE)
    index = (ROOT / "docs/llms.txt").read_text()
    for path in paths:
        route = "/k8s-launch-kit/" + ("" if path == "index.md" else path[:-3] + "/")
        if route not in index:
            raise ValueError(f"llms.txt is missing {route}")
    print(f"Navigation: all {len(paths)} published pages are listed in llms.txt")


def check_offline_examples(binary):
    reference = (ROOT / "docs/reference/configuration.md").read_text()
    profile = reference.split("## Profile\n", 1)[1].split("```yaml\n", 1)[1].split("```", 1)[0]
    with tempfile.TemporaryDirectory(prefix="l8k-doc-examples-") as scratch:
        base = Path(scratch)
        source = base / "profile.yaml"
        source.write_text(profile)
        common = ["generate", "--config-dir", scratch, "--user-config", str(source),
                  "--for", "PowerEdge-XE9680-H200", "--node-selector", "rack=42",
                  "--output", "json"]
        output = base / "deployment"
        result = json.loads(run(binary, *common, "--save-deployment-files", str(output)))
        if not result["success"] or source.read_text() != profile:
            raise ValueError("documented Profile YAML must render without changing its source")
        if not (output / ".l8k/resolved-config.yaml").is_file():
            raise ValueError("documented effective-config sidecar is missing")
        workload = base / "workload.yaml"
        workload.write_text("apiVersion: v1\nkind: Pod\nmetadata:\n  name: docs-example\n"
                            "spec:\n  containers:\n    - name: app\n      image: example.invalid/app:latest\n")
        run(binary, *common, "--save-deployment-files", str(output),
            "--workload-manifest", str(workload), "--network-namespaces", "training,inference")
        resources = output / "network-operator"
        custom = list(resources.glob("90-workload*.yaml"))
        if len(custom) != 1 or "namespace: training" not in custom[0].read_text():
            raise ValueError("documented Kubernetes custom-workload namespace behavior changed")
        if list(resources.glob("*example*.yaml")):
            raise ValueError("custom workload no longer replaces default connectivity fixtures")
    print("Offline rendering: published Profile YAML, unchanged source, sidecar, and custom workload passed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=ROOT / "build/l8k")
    args = parser.parse_args()
    binary = args.binary.resolve()
    try:
        check_syntax(binary)
        check_index()
        check_offline_examples(binary)
    except (ValueError, OSError, subprocess.TimeoutExpired) as error:
        parser.exit(1, f"Documentation check failed: {error}\n")


if __name__ == "__main__":
    main()
