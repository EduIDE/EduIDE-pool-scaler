"""A small DSL for hand-placed UML diagrams, written as draw.io files and exported to SVG.

Every shape gets explicit coordinates, so a layout stays exactly as designed instead of
depending on an auto-layout engine. Copy this file next to the project's diagram script:

    import drawio_dsl as dsl
    from drawio_dsl import *
    dsl.OUT_DIR = Path(__file__).resolve().parent

    d = Diagram("components")
    ...
    d.save()          # writes components.drawio and components.svg into OUT_DIR

Run the project script with `--png <dir>` to also write PNG previews for checking; the
script prints how diagram coordinates map to preview pixels, for cropping tight spots.
Needs the draw.io desktop CLI (`drawio`) on the PATH.
"""

import html
import subprocess
import sys
from pathlib import Path
from xml.sax.saxutils import quoteattr

OUT_DIR = Path.cwd()

# Colors carry meaning; every diagram that uses them gets a legend().
INK = "#1F1F1F"            # lines, borders, text
ACCENT = "#6D28D9"         # own subsystem names; technology text in deployment diagrams
ACCENT_LINE = "#7C3AED"    # own subsystem borders, namespace groups
ACCENT_FILL = "#F5F3FF"    # own subsystem fill
EXTERNAL_FILL = "#F4F4F5"  # external systems
MUTED = "#52525B"          # responsibility text in component diagrams
NOTE_FILL = "#FFFBEA"
FONT = "fontFamily=Helvetica;fontSize=13;fontColor=#1F1F1F;"


class Diagram:
    def __init__(self, name):
        self.name = name  # may contain a sub folder, e.g. "services/cost"
        self.cells = []
        self.next_id = 2
        self.min_x = self.min_y = float("inf")  # top left of the drawing, for preview pixel mapping

    def _extent(self, x, y):
        self.min_x, self.min_y = min(self.min_x, x), min(self.min_y, y)

    def _id(self, wanted=None):
        if wanted:
            return wanted
        self.next_id += 1
        return f"c{self.next_id}"

    def vertex(self, x, y, w, h, value, style, id=None, parent="1"):
        cid = self._id(id)
        if parent == "1":
            self._extent(x, y)
        self.cells.append(
            f'<mxCell id="{cid}" value={quoteattr(value)} style="{style}" vertex="1" parent="{parent}">'
            f'<mxGeometry x="{x}" y="{y}" width="{w}" height="{h}" as="geometry"/></mxCell>'
        )
        return cid

    def edge(self, src, tgt, style, points=(), exit=None, entry=None, labels=(), value=""):
        """exit/entry: relative (x, y) on the source/target box, e.g. (1, 0.5) = middle of the right side.
        labels: (text, position from -1 at source to 1 at target, dx, dy, align)."""
        cid = self._id()
        if exit:
            style += f"exitX={exit[0]};exitY={exit[1]};exitDx=0;exitDy=0;"
        if entry:
            style += f"entryX={entry[0]};entryY={entry[1]};entryDx=0;entryDy=0;"
        pts = "".join(f'<mxPoint x="{x}" y="{y}"/>' for x, y in points)
        pts = f'<Array as="points">{pts}</Array>' if pts else ""
        self.cells.append(
            f'<mxCell id="{cid}" value={quoteattr(value)} style="{style}" edge="1" parent="1" source="{src}" target="{tgt}">'
            f'<mxGeometry relative="1" as="geometry">{pts}</mxGeometry></mxCell>'
        )
        for text, pos, dx, dy, align in labels:
            self.cells.append(
                f'<mxCell id="{self._id()}" value={quoteattr(text)} '
                f'style="edgeLabel;html=1;resizable=0;points=[];labelBackgroundColor=none;align={align};{FONT}fontSize=12;" '
                f'vertex="1" connectable="0" parent="{cid}">'
                f'<mxGeometry x="{pos}" relative="1" as="geometry"><mxPoint x="{dx}" y="{dy}" as="offset"/></mxGeometry></mxCell>'
            )
        return cid

    def line(self, x1, y1, x2, y2, style, points=()):
        """An edge that is not attached to any shape."""
        cid = self._id()
        for px, py in [(x1, y1), (x2, y2), *points]:
            self._extent(px, py)
        pts = "".join(f'<mxPoint x="{x}" y="{y}"/>' for x, y in points)
        pts = f'<Array as="points">{pts}</Array>' if pts else ""
        self.cells.append(
            f'<mxCell id="{cid}" style="{style}" edge="1" parent="1">'
            f'<mxGeometry relative="1" as="geometry">'
            f'<mxPoint x="{x1}" y="{y1}" as="sourcePoint"/><mxPoint x="{x2}" y="{y2}" as="targetPoint"/>{pts}'
            f"</mxGeometry></mxCell>"
        )
        return cid

    def text(self, x, y, w, h, value, align="left", size=12, bold=False):
        style = f"text;html=1;align={align};verticalAlign=middle;whiteSpace=wrap;{FONT}fontSize={size};"
        if bold:
            style += "fontStyle=1;"
        return self.vertex(x, y, w, h, value, style)

    def xml(self):
        body = "".join(self.cells)
        return (
            f'<mxfile host="diagrams.py"><diagram id="{self.name}" name="{self.name}">'
            f'<mxGraphModel grid="0" page="0" math="0" shadow="0" background="#FFFFFF" adaptiveColors="none"><root>'
            f'<mxCell id="0"/><mxCell id="1" parent="0"/>{body}</root></mxGraphModel></diagram></mxfile>'
        )

    def save(self):
        drawio = OUT_DIR / f"{self.name}.drawio"
        drawio.parent.mkdir(parents=True, exist_ok=True)
        drawio.write_text(self.xml())
        svg = OUT_DIR / f"{self.name}.svg"
        # --embed-diagram keeps the SVG editable in draw.io
        subprocess.run(
            ["drawio", "-x", "-f", "svg", "--embed-diagram", "-b", "16", "-o", str(svg), str(drawio)],
            check=True,
            capture_output=True,
        )
        # draw.io exports adaptive light-dark() colors; pin the light variant and a white background
        text = svg.read_text()
        text = text.replace("color-scheme: light dark;", "color-scheme: light;")
        text = text.replace("background: transparent; background-color: transparent;",
                            "background: #FFFFFF; background-color: #FFFFFF;")
        svg.write_text(text)
        print(f"wrote {drawio.name} and {svg.name}")
        if "--png" in sys.argv:
            png_dir = Path(sys.argv[sys.argv.index("--png") + 1])
            png_dir.mkdir(parents=True, exist_ok=True)
            # sub folder names become part of the file name, so previews never collide
            png = png_dir / f"{self.name.replace('/', '-')}.png"
            subprocess.run(
                ["drawio", "-x", "-f", "png", "-s", "2", "-b", "16", "-o", str(png), str(drawio)],
                check=True,
                capture_output=True,
            )
            print(f"  preview {png}: pixel = 2 * (coordinate - ({self.min_x:g}, {self.min_y:g}) + 16)")


# ---------- edge styles ----------

LINE = f"html=1;rounded=0;endArrow=none;strokeColor={INK};strokeWidth=1.5;edgeStyle=orthogonalEdgeStyle;"
# required interface: the half circle wraps the ball it ends on
SOCKET = (f"html=1;rounded=0;endArrow=halfCircle;endFill=0;endSize=14;strokeColor={INK};strokeWidth=1.5;"
          f"edgeStyle=orthogonalEdgeStyle;")
ASSOC = f"html=1;rounded=0;endArrow=none;strokeColor={INK};strokeWidth=1.5;edgeStyle=orthogonalEdgeStyle;"
COMPOSITION = (f"html=1;rounded=0;startArrow=diamondThin;startFill=1;startSize=16;endArrow=none;"
               f"strokeColor={INK};strokeWidth=1.5;edgeStyle=orthogonalEdgeStyle;")
DEPENDENCY = (f"html=1;rounded=0;dashed=1;endArrow=open;endSize=12;strokeColor={INK};strokeWidth=1.5;"
              f"edgeStyle=orthogonalEdgeStyle;")
DASHED = f"html=1;rounded=0;dashed=1;endArrow=none;strokeColor={INK};strokeWidth=1.5;"
NOTE = (f"shape=note;size=14;html=1;whiteSpace=wrap;align=left;spacingLeft=8;spacingRight=8;"
        f"strokeColor={INK};fillColor={NOTE_FILL};{FONT}fontSize=12;")


# ---------- building blocks ----------

def esc(s):
    return html.escape(s, quote=False)


def frac(box, y):
    """Relative y position on a box's side for an absolute y. box = (x, y, w, h)."""
    return round((y - box[1]) / box[3], 4)


def note(d, x, y, w, h, text):
    return d.vertex(x, y, w, h, text, NOTE)


def uml_class(d, x, y, w, name, attributes=(), methods=(), stereotype=None, external=False):
    """UML class box; height follows from the number of lines. Returns (id, (x, y, w, h))."""
    line_h = 18
    head = f'<div style="padding:6px 0 4px 0;text-align:center;">'
    if stereotype:
        head += f'<span style="font-size:11px;">«{esc(stereotype)}»</span><br/>'
    head += f'<b>{esc(name)}</b></div>'
    parts = [head]
    # an interface usually has operations only; skip empty compartments
    sections = [sec for sec in (attributes, methods) if sec] or [()]
    for section in sections:
        rows = "<br/>".join(esc(r) for r in section)
        parts.append(f'<hr size="1" style="border-style:solid;border-color:{INK};margin:0;"/>'
                     f'<div style="padding:4px 10px;line-height:{line_h}px;">{rows}</div>')
    header_h = 46 if stereotype else 30
    h = header_h + sum(len(s) * line_h + 10 for s in sections)
    fill = EXTERNAL_FILL if external else "#FFFFFF"
    style = (f"html=1;verticalAlign=top;align=left;overflow=fill;whiteSpace=wrap;rounded=0;"
             f"strokeColor={INK};strokeWidth=1.5;fillColor={fill};{FONT}")
    return d.vertex(x, y, w, h, "".join(parts), style), (x, y, w, h)


def hassoc(d, a, a_box, b, b_box, y, left="", name="", right="", style=ASSOC, name_x=None):
    """Straight horizontal association at height y from a (left) to b (right).
    Multiplicities and the italic name sit above the line, never on it."""
    x1 = a_box[0] + a_box[2]
    x2 = b_box[0]
    d.edge(a, b, style, exit=(1, frac(a_box, y)), entry=(0, frac(b_box, y)))
    if left:
        d.text(x1 + 6, y - 22, 60, 20, left)
    if right:
        d.text(x2 - 66, y - 22, 60, 20, right, align="right")
    if name:
        cx = name_x if name_x is not None else (x1 + x2) / 2
        d.text(cx - 70, y - 22, 140, 20, f"<i>{name}</i>", align="center")


def component_icon(d, x, y, w):
    """Small UML component symbol in the top right corner of a box."""
    d.vertex(x + w - 30, y + 10, 16, 18, "",
             f"shape=module;jettyWidth=6;jettyHeight=3;strokeColor={INK};fillColor=#FFFFFF;html=1;")


def component(d, x, y, w, h, name, detail=None, stereotype="component", external=False):
    """UML component. The grey detail line names responsibilities, never technologies."""
    label = f'<span style="font-size:12px;">«{esc(stereotype)}»</span><br/><b style="font-size:15px;">{esc(name)}</b>'
    if detail:
        label += f'<br/><span style="color:{MUTED};font-size:12px;">{esc(detail)}</span>'
    fill = EXTERNAL_FILL if external else "#FFFFFF"
    cid = d.vertex(x, y, w, h, label,
                   f"html=1;whiteSpace=wrap;rounded=0;strokeColor={INK};strokeWidth=1.5;fillColor={fill};{FONT}")
    component_icon(d, x, y, w)
    return cid


def subsystem(d, x, y, w, h, name, size=18, stereotype="subsystem"):
    """Own subsystem: purple container with the title top left. Use "\\n" in name for a line break."""
    label = (f'<span style="font-size:12px;">«{esc(stereotype)}»</span><br/>'
             f'<b style="font-size:{size}px;color:{ACCENT};">{esc(name).replace(chr(10), "<br/>")}</b>')
    cid = d.vertex(x, y, w, h, label,
                   f"html=1;whiteSpace=wrap;rounded=0;verticalAlign=top;align=left;spacingLeft=16;spacingTop=8;"
                   f"strokeColor={ACCENT_LINE};strokeWidth=1.5;fillColor={ACCENT_FILL};{FONT}")
    component_icon(d, x, y, w)
    return cid


def ball(d, cx, cy, label, label_side="right", label_w=170):
    """Provided interface (lollipop ball) centred on (cx, cy). Pass label="" and place the
    label with d.text() when none of the four sides is free of lines."""
    r = 11
    cid = d.vertex(cx - r, cy - r, 2 * r, 2 * r, "",
                   f"ellipse;html=1;strokeColor={INK};strokeWidth=1.5;fillColor=#FFFFFF;")
    if label_side == "right":
        d.text(cx + 20, cy - 20, label_w, 40, label, size=13, bold=True)
    elif label_side == "left":
        d.text(cx - 20 - label_w, cy - 20, label_w, 40, label, align="right", size=13, bold=True)
    elif label_side == "below":
        d.text(cx - label_w / 2, cy + 14, label_w, 40, label, align="center", size=13, bold=True)
    elif label_side == "above":
        d.text(cx - label_w / 2, cy - 54, label_w, 40, label, align="center", size=13, bold=True)
    return cid


def actor(d, x, y, name, w=30, h=56):
    """UML actor for users outside the model (e.g. visitors of a public service). Name below."""
    return d.vertex(x, y, w, h, esc(name),
                    f"shape=umlActor;verticalLabelPosition=bottom;verticalAlign=top;html=1;"
                    f"strokeColor={INK};strokeWidth=1.5;fillColor=#FFFFFF;{FONT}")


def node(d, x, y, w, h, stereotype, name, tech=None, fill="#FFFFFF", stroke=INK, name_color=INK):
    """UML deployment node (3D box), label top left. tech is HTML, shown in the technology color.
    Leave 80 px between the node top and its first child (stereotype, name, one tech line);
    add 20 px per extra tech line. Attach paths to the node border, not to children."""
    label = f'<span style="font-size:11px;">«{esc(stereotype)}»</span><br/><b style="color:{name_color};">{esc(name)}</b>'
    if tech:
        label += f'<br/><span style="color:{ACCENT};font-size:11px;">{tech}</span>'
    return d.vertex(x, y, w, h, label,
                    f"shape=cube;size=12;flipH=1;html=1;whiteSpace=wrap;verticalAlign=top;align=left;"
                    f"spacingLeft=10;spacingTop=14;strokeColor={stroke};strokeWidth=1.5;fillColor={fill};{FONT}")


def group(d, x, y, w, h, title):
    """Flat dashed group (e.g. a Kubernetes namespace); keeps node nesting shallow."""
    d.vertex(x, y, w, h, "", f"html=1;rounded=0;dashed=1;strokeColor={ACCENT_LINE};strokeWidth=1.5;fillColor=none;")
    d.text(x + 14, y + 6, w - 28, 24, f'<span style="color:{ACCENT};">{title}</span>', size=12)


def artifact(d, x, y, w, h, name, detail=None):
    label = f'<span style="font-size:11px;">«artifact»</span><br/><b>{esc(name)}</b>'
    if detail:
        label += f'<br/><span style="color:{ACCENT};font-size:11px;">{detail}</span>'
    cid = d.vertex(x, y, w, h, label,
                   f"html=1;whiteSpace=wrap;rounded=0;strokeColor={INK};strokeWidth=1.5;fillColor=#FFFFFF;{FONT}fontSize=12;")
    d.vertex(x + w - 22, y + 7, 12, 15, "", f"shape=note;size=5;html=1;strokeColor={INK};fillColor=#FFFFFF;")
    return cid


def legend(d, x, y, w, entries):
    """Legend box. entries: (kind, text) with kind in subsystem, component, external,
    namespace, interface, technology, responsibility, note."""
    row = 28
    h = 40 + row * len(entries)
    d.vertex(x, y, w, h, "", "html=1;rounded=0;strokeColor=#A1A1AA;strokeWidth=1;fillColor=#FFFFFF;")
    d.text(x + 12, y + 6, w - 24, 24, "<b>Legend</b>", size=13)
    for i, (kind, text) in enumerate(entries):
        ry = y + 36 + i * row
        sx, sw, sh = x + 14, 34, 18
        if kind == "subsystem":
            d.vertex(sx, ry, sw, sh, "", f"html=1;strokeColor={ACCENT_LINE};strokeWidth=1.5;fillColor={ACCENT_FILL};")
        elif kind == "component":
            d.vertex(sx, ry, sw, sh, "", f"html=1;strokeColor={INK};strokeWidth=1.5;fillColor=#FFFFFF;")
        elif kind == "external":
            d.vertex(sx, ry, sw, sh, "", f"html=1;strokeColor={INK};strokeWidth=1.5;fillColor={EXTERNAL_FILL};")
        elif kind == "namespace":
            d.vertex(sx, ry, sw, sh, "", f"html=1;dashed=1;strokeColor={ACCENT_LINE};strokeWidth=1.5;fillColor=none;")
        elif kind == "note":
            d.vertex(sx, ry, sw, sh, "", f"shape=note;size=6;html=1;strokeColor={INK};fillColor={NOTE_FILL};")
        elif kind == "interface":
            d.line(sx, ry + sh / 2, sx + 18, ry + sh / 2, SOCKET.replace("endSize=14", "endSize=10"))
            d.vertex(sx + 20, ry + sh / 2 - 7, 14, 14, "",
                     f"ellipse;html=1;strokeColor={INK};strokeWidth=1.5;fillColor=#FFFFFF;")
        elif kind == "technology":
            d.text(sx, ry, sw + 6, sh, f'<span style="color:{ACCENT};">Go</span>', size=12)
        elif kind.startswith("«"):
            # A stereotype that marks the build status, e.g. «draft».
            d.text(sx - 4, ry, sw + 34, sh, f'<span style="font-size:11px;">{esc(kind)}</span>', size=11)
        elif kind == "responsibility":
            d.text(sx, ry, sw + 6, sh, f'<span style="color:{MUTED};">text</span>', size=12)
        # Stereotype samples are wider than the colour swatches.
        tx = sx + sw + (30 if kind.startswith("«") else 12)
        d.text(tx, ry - 3, w - (tx - x) - 14, sh + 6, text, size=12)


def service_spec(file_name, name, provider, operations, data=()):
    """Detailed UML view of one service: providing component, the «interface» with its
    operations, and its data types. data: [(type name, [attributes])]."""
    d = Diagram(file_name)
    prov = component(d, 40, 40, 220, 80, provider)
    lolli = ball(d, 330, 80, "")
    d.edge(prov, lolli, LINE, exit=(1, 0.5), entry=(0, 0.5))
    # wide enough for the longest operation on one line (about 5.6 px per character at 13 px)
    width = max(380, round(max((len(o) for o in operations), default=0) * 5.6 + 30))
    iface, (ix, iy, iw, ih) = uml_class(d, 420, 40, width, name, [], operations, stereotype="interface")
    d.line(341, 80, 420, 80, DASHED)
    data_x = 420 + width + 80
    y = 40
    for type_name, attributes in data:
        cls, (cx, cy, cw, ch) = uml_class(d, data_x, y, 280, type_name, attributes)
        d.edge(iface, cls, DEPENDENCY, exit=(1, round(min(40, ih / 2) / ih, 4)), entry=(0, 0.5),
               points=[(data_x - 40, iy + min(40, ih / 2)), (data_x - 40, cy + ch / 2)])
        y += ch + 30
    d.save()
