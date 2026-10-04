"""Generate the small document fixtures for internal/infra/extract tests."""

import io
import sys
from pathlib import Path

from docx import Document
from openpyxl import Workbook
from pptx import Presentation
from pptx.util import Inches
from pypdf import PdfReader, PdfWriter
from reportlab.lib.pagesizes import A4
from reportlab.pdfgen import canvas

out = Path(sys.argv[1])
out.mkdir(parents=True, exist_ok=True)


def pdf(pages, path, metadata=True):
    buf = io.BytesIO()
    c = canvas.Canvas(buf, pagesize=A4, invariant=1)
    for lines in pages:
        y = 800
        for line in lines:
            c.drawString(72, y, line)
            y -= 16
        if not lines:
            # An image-only page: a filled rectangle and no text layer.
            c.rect(72, 400, 300, 300, fill=1)
        c.showPage()
    c.save()
    path.write_bytes(buf.getvalue())


pdf(
    [
        ["Quarterly report FIXTURETOKENPDF1", "Revenue grew in every region during the quarter."],
        ["Second page discusses costs and the FIXTURETOKENPDF2 marker in detail here."],
        [],
        ["Fourth page closes the report with a summary paragraph for the reader."],
    ],
    out / "report.pdf",
)

# Encrypted with a user password: no reader can open it without that password.
reader = PdfReader(out / "report.pdf")
writer = PdfWriter()
for page in reader.pages:
    writer.add_page(page)
writer.encrypt(user_password="secret-user", owner_password="secret-owner", algorithm="AES-128")
with open(out / "encrypted.pdf", "wb") as handle:
    writer.write(handle)

doc = Document()
doc.add_heading("Introduction", level=1)
doc.add_paragraph("The docx body carries FIXTURETOKENDOCX in its first section.")
doc.add_heading("Details", level=1)
doc.add_paragraph("A second section follows the heading.")
table = doc.add_table(rows=2, cols=2)
table.cell(0, 0).text = "name"
table.cell(0, 1).text = "value"
table.cell(1, 0).text = "alpha"
table.cell(1, 1).text = "42"
doc.save(out / "notes.docx")

wb = Workbook()
ws = wb.active
ws.title = "Sales"
ws.append(["region", "amount"])
ws.append(["north", 120])
ws.append(["south", "FIXTURETOKENXLSX"])
ws2 = wb.create_sheet("Empty")
ws3 = wb.create_sheet("Totals")
ws3.append(["total", 240])
wb.save(out / "sheet.xlsx")

prs = Presentation()
for index, (title, body) in enumerate(
    [("Opening slide", "FIXTURETOKENPPTX1 appears here"), ("Closing slide", "FIXTURETOKENPPTX2 ends it")]
):
    slide = prs.slides.add_slide(prs.slide_layouts[1])
    slide.shapes.title.text = title
    slide.placeholders[1].text = body
prs.save(out / "deck.pptx")
print("ok")
