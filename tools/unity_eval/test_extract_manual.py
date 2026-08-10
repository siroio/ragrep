import contextlib
from datetime import datetime, timezone
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from extract_manual import ExtractionError, convert_manual, extract_markdown, main


class ExtractMarkdownTests(unittest.TestCase):
    def test_extracts_only_manual_content_section(self):
        html = """<!doctype html><html><body>
        <div id="sidebar">SIDEBAR NOISE</div>
        <div id="content-wrap"><div class="content"><div class="section">
          <div class="nextprev">PREVIOUS NOISE</div>
          <h1>Static batching</h1>
          <p>Combine <a href="other.html">static meshes</a> at build time.</p>
          <script>TRACKING NOISE</script>
        </div></div></div>
        </body></html>"""

        got = extract_markdown(html, "Manual/static-batching.html", "6000.3.11f1")

        self.assertIn("# Static batching", got)
        self.assertIn("Combine static meshes at build time.", got)
        self.assertNotIn("SIDEBAR NOISE", got)
        self.assertNotIn("PREVIOUS NOISE", got)
        self.assertNotIn("TRACKING NOISE", got)
        self.assertTrue(got.startswith("---\nsource: Manual/static-batching.html\nunity_version: 6000.3.11f1\n---\n"))

    def test_skips_breadcrumbs_at_start_of_manual_section(self):
        html = """<div id="content-wrap"><div class="section">
        <div class="breadcrumbs clear"><ul><li>Getting started</li><li>Graphics</li></ul></div>
        <h1>Static batching</h1>
        <p>Combine meshes at build time.</p>
        </div></div>"""

        got = extract_markdown(html, "Manual/static-batching.html", "6000.3.11f1")

        self.assertIn("# Static batching", got)
        self.assertIn("Combine meshes at build time.", got)
        self.assertNotIn("Getting started", got)
        self.assertNotIn("Graphics", got)

    def test_renders_lists_tables_and_code(self):
        html = """<div id="content-wrap"><div class="section">
        <h2>Requirements</h2><ul><li>First</li><li>Second</li></ul>
        <table><tr><th>Mode</th><th>Value</th></tr><tr><td>Fast</td><td>1</td></tr></table>
        <pre><code>if (ready) {
    Run();
}</code></pre>
        </div></div>"""
        got = extract_markdown(html, "Manual/example.html", "6000.3.11f1")
        self.assertIn("## Requirements", got)
        self.assertIn("- First\n- Second", got)
        self.assertIn("| Mode | Value |", got)
        self.assertIn("```\nif (ready) {\n    Run();\n}\n```", got)

    def test_skips_table_rows_in_excluded_subtrees(self):
        html = """<div id="content-wrap"><div class="section">
        <table>
          <tr><th>Mode</th><th>Value</th></tr><tr><td>Fast</td><td>1</td></tr>
          <tbody class="nextprev"><tr><td>PREVIOUS</td><td>NOISE</td></tr></tbody>
          <script><tr><td>SCRIPT</td><td>NOISE</td></tr></script>
          <style><tr><td>STYLE</td><td>NOISE</td></tr></style>
        </table>
        </div></div>"""
        got = extract_markdown(html, "Manual/table.html", "6000.3.11f1")
        self.assertIn("| Fast | 1 |", got)
        self.assertNotIn("PREVIOUS", got)
        self.assertNotIn("SCRIPT", got)
        self.assertNotIn("STYLE", got)

    def test_renders_nested_lists(self):
        html = """<div id="content-wrap"><div class="section">
        <ul><li>Parent<ol><li>Child</li></ol></li></ul>
        </div></div>"""
        got = extract_markdown(html, "Manual/lists.html", "6000.3.11f1")
        self.assertIn("- Parent\n  1. Child", got)

    def test_skips_section_nested_in_nextprev(self):
        html = """<div id="content-wrap">
        <div class="section"><p>Manual body</p></div>
        <div class="nextprev"><div class="section"><p>PREVIOUS NOISE</p></div></div>
        </div>"""
        got = extract_markdown(html, "Manual/nextprev.html", "6000.3.11f1")
        self.assertIn("Manual body", got)
        self.assertNotIn("PREVIOUS NOISE", got)

    def test_collapses_source_line_breaks_but_preserves_br(self):
        html = """<div id="content-wrap"><div class="section">
        <p>Combine
          static<br>meshes.</p>
        </div></div>"""
        got = extract_markdown(html, "Manual/whitespace.html", "6000.3.11f1")
        self.assertIn("Combine static\nmeshes.", got)

    def test_rejects_missing_content_section(self):
        with self.assertRaisesRegex(ExtractionError, "missing #content-wrap"):
            extract_markdown("<p>body</p>", "Manual/bad.html", "6000.3.11f1")

    def test_rejects_newline_in_frontmatter_value(self):
        html = '<div id="content-wrap"><div class="section"><p>body</p></div></div>'
        with self.assertRaisesRegex(ExtractionError, "newline"):
            extract_markdown(html, "Manual/bad\nname.html", "6000.3.11f1")

    def test_rejects_newline_in_unity_version(self):
        html = '<div id="content-wrap"><div class="section"><p>body</p></div></div>'
        with self.assertRaisesRegex(ExtractionError, "newline"):
            extract_markdown(html, "Manual/bad.html", "6000.3\r11f1")

    def test_rejects_empty_content_section(self):
        html = '<div id="content-wrap"><div class="section"><script>noise</script></div></div>'
        with self.assertRaisesRegex(ExtractionError, "empty #content-wrap .section"):
            extract_markdown(html, "Manual/empty.html", "6000.3.11f1")


class ConvertManualTests(unittest.TestCase):
    def test_converts_tree_and_records_failures(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            source = base / "Manual"
            source.mkdir()
            valid = '<div id="content-wrap"><div class="section"><h1>{}</h1></div></div>'
            (source / "a.html").write_text(valid.format("A"), encoding="utf-8")
            (source / "b.html").write_text(valid.format("B"), encoding="utf-8")
            (source / "bad.html").write_text("<p>bad</p>", encoding="utf-8")
            output = base / "snapshot"

            manifest = convert_manual(source, output, "6000.3.11f1", datetime(2026, 8, 10, tzinfo=timezone.utc))

            self.assertEqual(manifest["source_files"], 3)
            self.assertEqual(manifest["converted"], 2)
            self.assertEqual(manifest["failed"], 1)
            self.assertEqual(manifest["empty"], 0)
            self.assertEqual(manifest["failures"][0]["source"], "Manual/bad.html")
            self.assertTrue((output / "corpus" / "Manual" / "a.md").is_file())
            disk_manifest = json.loads((output / "manifest.json").read_text(encoding="utf-8"))
            self.assertEqual(disk_manifest, manifest)

    def test_refuses_existing_output_root(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            source = base / "Manual"
            source.mkdir()
            output = base / "snapshot"
            output.mkdir()
            with self.assertRaises(FileExistsError):
                convert_manual(source, output, "6000.3.11f1", datetime.now(timezone.utc))

    def test_output_is_deterministic_for_matching_sources(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            html = '<div id="content-wrap"><div class="section"><h1>Stable</h1></div></div>'
            source_a = base / "source-a" / "Manual"
            source_b = base / "source-b" / "Manual"
            source_a.mkdir(parents=True)
            source_b.mkdir(parents=True)
            (source_a / "page.html").write_text(html, encoding="utf-8")
            (source_b / "page.html").write_text(html, encoding="utf-8")
            generated_at = datetime(2026, 8, 10, tzinfo=timezone.utc)

            manifest_a = convert_manual(source_a, base / "snapshot-a", "6000.3.11f1", generated_at)
            manifest_b = convert_manual(source_b, base / "snapshot-b", "6000.3.11f1", generated_at)

            markdown_a = (base / "snapshot-a" / "corpus" / "Manual" / "page.md").read_bytes()
            markdown_b = (base / "snapshot-b" / "corpus" / "Manual" / "page.md").read_bytes()
            self.assertEqual(markdown_a, markdown_b)
            manifest_a.pop("source_root")
            manifest_b.pop("source_root")
            self.assertEqual(manifest_a, manifest_b)

    def test_cli_prints_conversion_summary(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            source = base / "Manual"
            source.mkdir()
            (source / "page.html").write_text(
                '<div id="content-wrap"><div class="section"><h1>CLI</h1></div></div>',
                encoding="utf-8",
            )
            output = base / "snapshot"
            stdout = io.StringIO()

            with contextlib.redirect_stdout(stdout):
                result = main(["--source", str(source), "--output", str(output), "--unity-version", "6000.3.11f1"])

            self.assertEqual(result, 0)
            self.assertEqual(stdout.getvalue(), f"converted=1 failed=0 empty=0 output={output.resolve()}\n")

    def test_cli_returns_one_for_invalid_arguments(self):
        stderr = io.StringIO()

        with contextlib.redirect_stderr(stderr):
            result = main([])

        self.assertEqual(result, 1)
        self.assertIn("error:", stderr.getvalue())

    def test_refuses_output_created_before_publication(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            source = base / "Manual"
            source.mkdir()
            (source / "page.html").write_text(
                '<div id="content-wrap"><div class="section"><h1>Race</h1></div></div>',
                encoding="utf-8",
            )
            output = base / "snapshot"
            original_write_text = Path.write_text

            def create_output_after_manifest(path, data, *args, **kwargs):
                result = original_write_text(path, data, *args, **kwargs)
                if path.name == "manifest.json":
                    output.mkdir()
                return result

            def rename_must_not_run(path, target):
                raise AssertionError("publication attempted after output root appeared")

            with patch.object(Path, "write_text", create_output_after_manifest), patch.object(Path, "rename", rename_must_not_run):
                with self.assertRaises(FileExistsError):
                    convert_manual(source, output, "6000.3.11f1", datetime.now(timezone.utc))

            self.assertTrue(output.is_dir())
            self.assertEqual(list(base.glob(".snapshot-*")), [])

    def test_removes_temporary_output_when_publication_fails(self):
        with tempfile.TemporaryDirectory() as raw:
            base = Path(raw)
            source = base / "Manual"
            source.mkdir()
            (source / "page.html").write_text(
                '<div id="content-wrap"><div class="section"><h1>Cleanup</h1></div></div>',
                encoding="utf-8",
            )
            output = base / "snapshot"

            with patch.object(Path, "rename", side_effect=OSError("publication failed")):
                with self.assertRaisesRegex(OSError, "publication failed"):
                    convert_manual(source, output, "6000.3.11f1", datetime.now(timezone.utc))

            self.assertFalse(output.exists())
            self.assertEqual(list(base.glob(".snapshot-*")), [])


if __name__ == "__main__":
    unittest.main()
