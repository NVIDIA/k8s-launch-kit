# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0
"""Documentation generation contracts; no CLI or infrastructure is invoked."""

import copy
from contextlib import redirect_stderr
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("workflow_docs", Path(__file__).with_name("generate-workflow-docs.py"))
generator = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(generator)


class WorkflowDocsTests(unittest.TestCase):
    def setUp(self):
        self.doc = {"formatVersion": 1, "title": "Procedures", "introduction": "Read first.", "workflows": [
            {"id": "inventory", "title": "Inventory", "introduction": "Collect facts.", "steps": [
                {"id": "run", "title": "Run", "body": "```sh\nprintf 'example only'\n```", "transitions": [{"to": "inspect", "label": "complete"}]},
                {"id": "inspect", "title": "Inspect", "body": "Read the report.", "transitions": []}]}]}

    def test_deterministic_procedure_and_graph(self):
        original = copy.deepcopy(self.doc)
        rendered = generator.render(self.doc)
        self.assertEqual(rendered, generator.render(self.doc))
        self.assertEqual(self.doc, original)
        self.assertIn('n1 -->|"complete"| n2', rendered)
        self.assertIn("[complete](#inventory-inspect)", rendered)
        self.assertIn("printf 'example only'", rendered)
        self.assertIn("### 2. Inspect", rendered)

    def test_rejects_duplicate_ids_and_unknown_destinations(self):
        for change in (
            lambda d: d["workflows"].append(copy.deepcopy(d["workflows"][0])),
            lambda d: d["workflows"][0]["steps"].append(copy.deepcopy(d["workflows"][0]["steps"][0])),
            lambda d: d["workflows"][0]["steps"][0]["transitions"][0].update(to="absent"),
        ):
            doc = copy.deepcopy(self.doc)
            change(doc)
            with self.assertRaises(ValueError):
                generator.render(doc)

    def test_rejects_malformed_shapes_and_unknown_fields(self):
        for change in (
            lambda d: d.update(formatVersion=True),
            lambda d: d.update(workflows=[]),
            lambda d: d.update(command="must not execute"),
            lambda d: d["workflows"][0].update(steps=[]),
            lambda d: d["workflows"][0]["steps"][0].update(transitions="bad"),
        ):
            doc = copy.deepcopy(self.doc)
            change(doc)
            with self.assertRaises(ValueError):
                generator.render(doc)
        with self.assertRaises(ValueError):
            json.loads('{"id":"one", "id":"two"}', object_pairs_hook=generator.unique_keys)

    def test_escapes_diagram_labels(self):
        self.doc["workflows"][0]["steps"][0]["title"] = 'Read "report"\n<script>'
        self.assertIn('n1["Read &quot;report&quot; &lt;script&gt;"]', generator.render(self.doc))

    def test_check_detects_drift_without_writing(self):
        with tempfile.TemporaryDirectory() as directory, redirect_stderr(io.StringIO()):
            source, output = Path(directory) / "workflow.json", Path(directory) / "guide.md"
            source.write_text(json.dumps(self.doc), encoding="utf-8")
            args = ["--source", str(source), "--output", str(output)]
            self.assertEqual(generator.main(args + ["--check"]), 1)
            self.assertFalse(output.exists())
            self.assertEqual(generator.main(args), 0)
            self.assertEqual(generator.main(args + ["--check"]), 0)
            output.write_text("stale", encoding="utf-8")
            self.assertEqual(generator.main(args + ["--check"]), 1)
            self.assertEqual(output.read_text(encoding="utf-8"), "stale")
            source.write_text('{"broken": true}', encoding="utf-8")
            self.assertEqual(generator.main(args), 1)
            self.assertEqual(output.read_text(encoding="utf-8"), "stale")


if __name__ == "__main__":
    unittest.main()
