# SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0
"""Refresh the reviewed Launch Kit snapshot of the NVIDIA AI Enterprise matrix."""

import argparse
from datetime import datetime, timezone
import hashlib
from html.parser import HTMLParser
import json
from pathlib import Path
import re
import tempfile
import urllib.error
import urllib.request

BASE = "https://docs.nvidia.com/ai-enterprise/support-matrix/latest/"
PINNED = "https://docs.nvidia.com/ai-enterprise/release-9/latest/support/support-matrix-9/9.0.html"
RELEASE = "9.0"
COMPONENTS = {
    "Base Command Manager (BCM)", "Container Toolkit", "DOCA Driver for Networking",
    "DOCA Microservices", "DPU Operator (DPF)", "Data Center GPU Driver",
    "Fabric Manager", "GPU Operator", "NIM Operator", "Network Operator",
    "Run:ai", "Virtual GPU Manager", "vGPU for Compute Guest Driver",
}
LIMIT = 2 * 1024 * 1024


class MatrixIndex(HTMLParser):
    def __init__(self):
        super().__init__()
        self.scripts = []

    def handle_starttag(self, tag, attrs):
        if tag != "script":
            return
        for key, value in attrs:
            if key == "src" and value and re.fullmatch(r"data/matrix_data\.[a-f0-9]{8,64}\.js", value):
                self.scripts.append(value)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "Redirect rejected", headers, fp)


def fetch(url):
    with urllib.request.build_opener(NoRedirect).open(url, timeout=15) as response:
        data = response.read(LIMIT + 1)
    if len(data) > LIMIT:
        raise ValueError(f"matrix asset exceeds {LIMIT} bytes: {url}")
    return data


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError(f"invalid JSON constant: {value}")


def arrays(data):
    decoder = json.JSONDecoder(object_pairs_hook=unique_object, parse_constant=invalid_constant)
    declarations = {}
    position = 0
    source = data.decode("utf-8")
    while match := re.match(r"\s*const\s+([a-zA-Z][a-zA-Z0-9]*)\s*=\s*", source[position:]):
        name = match.group(1)
        if name in declarations:
            raise ValueError(f"duplicate matrix declaration: {name}")
        value, end = decoder.raw_decode(source, position + match.end())
        terminator = re.match(r"\s*;", source[end:])
        if not terminator or not isinstance(value, list) or not all(isinstance(row, dict) for row in value):
            raise ValueError(f"invalid matrix array: {name}")
        declarations[name] = value
        position = end + terminator.end()
    # The generated bundle ends in one optional CommonJS export.
    if not re.fullmatch(r"\s*(?:if\(typeof module!=='undefined'&&module\.exports\)\{module\.exports=\{[a-zA-Z0-9,]+\};\})?\s*", source[position:]):
        raise ValueError("unexpected source after matrix arrays")
    return declarations


def required_strings(row, fields):
    for field in fields:
        if not isinstance(row[field], str) or not row[field].strip():
            raise ValueError(f"invalid {field} in row {row}")


def release_rows(rows):
    for row in rows:
        if not isinstance(row["versions"], list) or not row["versions"] or not all(
            isinstance(version, str) and re.fullmatch(r"[0-9]+\.[0-9]+", version) for version in row["versions"]
        ):
            raise ValueError(f"invalid release list in row {row}")
        if RELEASE in row["versions"]:
            yield {key: value for key, value in row.items() if key != "versions"}


def validate_version_offers(raw):
    for offer in raw.split(","):
        if not re.fullmatch(r"[0-9]+\.[0-9]+(?: - [0-9]+\.[0-9]+| LTS)?", offer.strip()):
            raise ValueError(f"unrecognized platform or OS version syntax: {raw}")


def normalize(index, data, data_url):
    tables = arrays(data)
    software = []
    seen = set()
    for row in release_rows(tables["infraSoftware"]):
        required_strings(row, ("name", "version90"))
        if row["name"] in seen or type(row["x86"]) is not bool or type(row["arm"]) is not bool:
            raise ValueError(f"duplicate component or invalid architecture flags: {row['name']}")
        seen.add(row["name"])
        software.append({"name": row["name"], "version": row["version90"], "x86": row["x86"], "arm": row["arm"]})
    if seen != COMPONENTS:
        raise ValueError(f"9.0 component set changed: {sorted(seen ^ COMPONENTS)}")

    gpus = list(release_rows(tables["supportedGPUs"]))
    for row in gpus:
        required_strings(row, ("name", "type", "arch"))
        exclusions = row.get("deploymentExclusions", [])
        if not isinstance(exclusions, list) or not all(isinstance(value, str) for value in exclusions):
            raise ValueError("invalid GPU deployment exclusions")
        row.pop("vgpuHypervisorExclusions", None)

    platforms = list(release_rows(tables["bareMetalK8s"]))
    for row in platforms:
        required_strings(row, ("platform", "version", "engine", "os", "gpuDriver", "gpuOp", "netOp", "dpuOp"))
        validate_version_offers(row["version"])
        os = re.fullmatch(r"(?:Red Hat Enterprise Linux|Red Hat CoreOS|Ubuntu|Rocky Linux|SLES) (.+)", row["os"])
        if not os or row["engine"] not in ("Containerd", "CRI-O") or row["gpuDriver"] not in (
            "vGPU Guest/Data Center", "Data Center", "vGPU Guest"
        ):
            raise ValueError(f"unrecognized operating system, runtime or GPU driver in row {row}")
        validate_version_offers(os.group(1))
        for field in ("footnotes", "netOpFootnotes"):
            if field in row and (not isinstance(row[field], list) or not all(
                type(value) is int and value > 0 for value in row[field]
            )):
                raise ValueError(f"invalid {field} in row {row}")
        for field in ("netOp", "gpuOp", "dpuOp"):
            if row[field] not in ("Supported", "-"):
                raise ValueError(f"unrecognized {field} value {row[field]}")
    if not gpus or not platforms:
        raise ValueError("missing GPU or platform rows")

    return {
        "schema": "l8k.nvaie.catalog.v1",
        "source": {
            "matrixURL": BASE + "index.html",
            "staticURL": PINNED,
            "dataURL": data_url,
            "retrievedAt": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "indexSHA256": hashlib.sha256(index).hexdigest(),
            "dataSHA256": hashlib.sha256(data).hexdigest(),
        },
        "releases": [{
            "version": RELEASE,
            "components": sorted(software, key=lambda row: row["name"]),
            "gpus": sorted(gpus, key=lambda row: row["name"]),
            "bareMetalK8s": sorted(platforms, key=lambda row: (row["platform"], row["os"], row["engine"])),
        }],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().parents[1] / "pkg/nvaie/catalog.json")
    args = parser.parse_args()
    index = fetch(BASE + "index.html")
    html = MatrixIndex()
    html.feed(index.decode("utf-8"))
    if len(html.scripts) != 1:
        raise ValueError("expected one same-origin matrix data script")
    data_url = BASE + html.scripts[0]
    catalog = normalize(index, fetch(data_url), data_url)
    temporary_path = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=args.output.parent, delete=False) as stream:
            temporary_path = Path(stream.name)
            stream.write(json.dumps(catalog, ensure_ascii=False, indent=2) + "\n")
        temporary_path.chmod(0o644)
        temporary_path.replace(args.output)
    finally:
        if temporary_path is not None:
            temporary_path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
