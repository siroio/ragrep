import unittest

from extract_manual import ExtractionError, extract_markdown


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


if __name__ == "__main__":
    unittest.main()
