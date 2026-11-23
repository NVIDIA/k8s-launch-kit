# SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0

from datetime import datetime, timezone
import importlib.util
import json
from tempfile import TemporaryDirectory
from pathlib import Path
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("update-nvaie-catalog.py")
SPEC = importlib.util.spec_from_file_location("update_nvaie_catalog", SCRIPT)
catalog_update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(catalog_update)


class CatalogRefreshTests(unittest.TestCase):
    def test_index_accepts_only_relative_fingerprinted_asset(self):
        index = catalog_update.MatrixIndex()
        index.feed(
            '<script src="https://example.com/data/matrix_data.36919e29.js"></script>'
            '<script src="../data/matrix_data.36919e29.js"></script>'
            '<script src="data/matrix_data.js"></script>'
            '<script src="data/matrix_data.36919e29.js" defer></script>'
        )
        self.assertEqual(index.scripts, ["data/matrix_data.36919e29.js"])

    def test_strict_json_extraction_at_adjacent_declarations(self):
        data = b'const infraSoftware=[{"name":"Network Operator"}];const supportedGPUs=[];'
        self.assertEqual(
            catalog_update.arrays(data), {"infraSoftware": [{"name": "Network Operator"}], "supportedGPUs": []}
        )
        cases = (
            (b'const infraSoftware=[];const infraSoftware=[];', "duplicate matrix declaration"),
            (b'const infraSoftware=[{"name":"A","name":"B"}];', "duplicate JSON key"),
            (b'const infraSoftware=[{"value":NaN}];', "invalid JSON constant"),
            (b'const infraSoftware=[{"value":2}]', "invalid matrix array"),
            (b'const infraSoftware=[];loadCustomCode();', "unexpected source"),
        )
        for invalid, reason in cases:
            with self.subTest(invalid=invalid), self.assertRaisesRegex(ValueError, reason):
                catalog_update.arrays(invalid)

    def test_explicit_version_pin_and_deterministic_output_for_fixed_time(self):
        source = self.sample_matrix()
        with patch.object(catalog_update, "datetime") as clock:
            clock.now.return_value = datetime(2026, 11, 20, tzinfo=timezone.utc)
            snapshot = catalog_update.normalize(b"index", source, "https://example.org/asset.js")
            again = catalog_update.normalize(b"index", source, "https://example.org/asset.js")
        self.assertEqual(json.dumps(snapshot), json.dumps(again))
        release = snapshot["releases"][0]
        self.assertEqual(release["version"], "9.0")
        self.assertEqual(len(release["components"]), 13)
        pin = next(item for item in release["components"] if item["name"] == "Network Operator")
        self.assertEqual(pin["version"], "26.7.1")
        with self.assertRaisesRegex(KeyError, "version90"):
            catalog_update.normalize(b"index", source.replace(b"version90", b"wrongVersion"), "https://example.org/data.js")

    def test_failed_refresh_preserves_existing_snapshot(self):
        html = b'<script src="data/matrix_data.36919e29.js"></script>'
        invalid_data = b'const infraSoftware=[];const infraSoftware=[];'
        with TemporaryDirectory() as directory:
            output = Path(directory) / "catalog.json"
            output.write_bytes(b"existing snapshot")
            with patch.object(catalog_update, "fetch", side_effect=[html, invalid_data]), patch(
                "sys.argv", [str(SCRIPT), "--output", str(output)]
            ), self.assertRaisesRegex(ValueError, "duplicate matrix declaration"):
                catalog_update.main()
            self.assertEqual(output.read_bytes(), b"existing snapshot")
            self.assertEqual(list(Path(directory).iterdir()), [output])

    def test_bundled_snapshot_matches_reviewed_scope(self):
        path = SCRIPT.parent.parent / "pkg/nvaie/catalog.json"
        snapshot = json.loads(path.read_text(encoding="utf-8"))
        release, = snapshot["releases"]
        self.assertEqual(release["version"], "9.0")
        self.assertEqual({component["name"] for component in release["components"]}, catalog_update.COMPONENTS)
        self.assertEqual(len(release["gpus"]), 47)
        self.assertEqual(len(release["bareMetalK8s"]), 10)

    @staticmethod
    def sample_matrix():
        tables = {
            "infraSoftware": [
                {"name": name, "version": "26.4.1", "version90": "26.7.1", "versions": ["9.0"], "x86": True, "arm": True}
                for name in sorted(catalog_update.COMPONENTS)
            ],
            "supportedGPUs": [{"name": "RTX PRO 6000 Blackwell Server Edition", "arch": "Blackwell", "type": "PCIe GPU Server", "versions": ["9.0"]}],
            "bareMetalK8s": [
                {
                    "gpuOp": "Supported", "netOp": "Supported", "dpuOp": "-", "engine": "Containerd",
                    "gpuDriver": "vGPU Guest/Data Center", "os": "Ubuntu 24.04 LTS", "platform": "SUSE Rancher RKE2",
                    "version": "1.33 - 1.37", "versions": ["9.0"],
                }
            ],
        }
        return "".join(f"const {name}={json.dumps(rows)};" for name, rows in tables.items()).encode("utf-8")


if __name__ == "__main__":
    unittest.main()
