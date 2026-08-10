from dataclasses import dataclass, field
from html.parser import HTMLParser
import re


VOID_TAGS = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"}
SKIP_TAGS = {"script", "style"}
SKIP_CLASSES = {"nextprev"}
BR_MARKER = "\0"


class ExtractionError(ValueError):
    pass


@dataclass
class Node:
    tag: str
    attrs: dict[str, str] = field(default_factory=dict)
    children: list["Node | str"] = field(default_factory=list)


class TreeParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.root = Node("document")
        self.stack = [self.root]

    def handle_starttag(self, tag, attrs):
        node = Node(tag.lower(), {key: value or "" for key, value in attrs})
        self.stack[-1].children.append(node)
        if node.tag not in VOID_TAGS:
            self.stack.append(node)

    def handle_startendtag(self, tag, attrs):
        self.handle_starttag(tag, attrs)
        if tag.lower() not in VOID_TAGS:
            self.stack.pop()

    def handle_endtag(self, tag):
        wanted = tag.lower()
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag == wanted:
                del self.stack[index:]
                return

    def handle_data(self, data):
        self.stack[-1].children.append(data)


def has_class(node: Node, name: str) -> bool:
    return name in node.attrs.get("class", "").split()


def descendants(node: Node):
    for child in node.children:
        if isinstance(child, Node):
            yield child
            yield from descendants(child)


def visible_descendants(node: Node):
    for child in node.children:
        if isinstance(child, Node) and not skipped(child):
            yield child
            yield from visible_descendants(child)


def top_level_sections(node: Node) -> list[Node]:
    found: list[Node] = []
    for child in node.children:
        if not isinstance(child, Node):
            continue
        if skipped(child):
            continue
        if has_class(child, "section"):
            found.append(child)
        else:
            found.extend(top_level_sections(child))
    return found


def skipped(node: Node) -> bool:
    return node.tag in SKIP_TAGS or any(has_class(node, name) for name in SKIP_CLASSES)


def text_content(node: Node, preserve: bool = False) -> str:
    parts: list[str] = []
    for child in node.children:
        if isinstance(child, str):
            parts.append(child)
        elif not skipped(child):
            if child.tag == "br":
                parts.append(BR_MARKER)
            elif child.tag == "code" and not preserve:
                parts.append(f"`{text_content(child, True)}`")
            else:
                parts.append(text_content(child, preserve))
    joined = "".join(parts)
    if preserve:
        return joined.replace(BR_MARKER, "\n").strip()
    return re.sub(r"\s+", " ", joined).replace(BR_MARKER, "\n").strip()


def render_list(node: Node, list_depth: int) -> list[str]:
    marker = "-" if node.tag == "ul" else "1."
    lines: list[str] = []
    for item in node.children:
        if not isinstance(item, Node) or item.tag != "li":
            continue
        inline_children = [
            child
            for child in item.children
            if not isinstance(child, Node) or child.tag not in {"ul", "ol"}
        ]
        value = text_content(Node("list-item", children=inline_children))
        if value:
            lines.append(f'{"  " * list_depth}{marker} {value}')
        for child in item.children:
            if isinstance(child, Node) and child.tag in {"ul", "ol"}:
                lines.extend(render_list(child, list_depth + 1))
    return lines


def render_node(node: Node, blocks: list[str], list_depth: int) -> None:
    if skipped(node):
        return
    if node.tag in {"h1", "h2", "h3", "h4", "h5", "h6"}:
        blocks.append(f'{"#" * int(node.tag[1])} {text_content(node)}')
        return
    if node.tag == "p":
        value = text_content(node)
        if value:
            blocks.append(value)
        return
    if node.tag in {"ul", "ol"}:
        rendered = render_list(node, list_depth)
        if rendered:
            blocks.append("\n".join(rendered))
        return
    if node.tag == "table":
        rows: list[list[str]] = []
        for candidate in visible_descendants(node):
            if candidate.tag != "tr":
                continue
            cells = [
                text_content(child).replace("|", "\\|")
                for child in candidate.children
                if isinstance(child, Node) and child.tag in {"th", "td"}
            ]
            if cells:
                rows.append(cells)
        if rows:
            width = len(rows[0])
            rows = [row for row in rows if len(row) == width]
            table = ["| " + " | ".join(row) + " |" for row in rows]
            table.insert(1, "| " + " | ".join(["---"] * width) + " |")
            blocks.append("\n".join(table))
        return
    if node.tag == "pre":
        blocks.append(f"```\n{text_content(node, True)}\n```")
        return
    for child in node.children:
        if isinstance(child, Node):
            render_node(child, blocks, list_depth)


def render_sections(sections: list[Node]) -> str:
    blocks: list[str] = []
    for section in sections:
        render_node(section, blocks, 0)
    return "\n\n".join(block for block in blocks if block.strip())


def extract_markdown(html_text: str, source: str, unity_version: str) -> str:
    parser = TreeParser()
    parser.feed(html_text)
    content = next((node for node in descendants(parser.root) if node.attrs.get("id") == "content-wrap"), None)
    if content is None:
        raise ExtractionError(f"missing #content-wrap: {source}")
    if "\r" in source or "\n" in source or "\r" in unity_version or "\n" in unity_version:
        raise ExtractionError("frontmatter value contains newline")
    sections = top_level_sections(content)
    if not sections:
        raise ExtractionError(f"missing #content-wrap .section: {source}")
    body = render_sections(sections).strip()
    if not body:
        raise ExtractionError(f"empty #content-wrap .section: {source}")
    return f"---\nsource: {source}\nunity_version: {unity_version}\n---\n\n{body}\n"
