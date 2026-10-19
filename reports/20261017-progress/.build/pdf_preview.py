from pathlib import Path
from reportlab.pdfgen.canvas import Canvas
from pypdf import PdfReader
from PIL import Image
import zipfile
import xml.etree.ElementTree as ET
base=Path(__file__).parent
out=base.parent/'output'
dest=out/'遥感变化检测平台_组会汇报_20261017.pdf'
c=Canvas(str(dest),pagesize=(960,540))
c.setTitle('遥感变化检测平台阶段进展汇报')
for i in range(1,13):
    f=base/f'final-{i:02}.png'
    c.drawImage(str(f),0,0,width=960,height=540)
    c.showPage()
c.save()
reader=PdfReader(dest)
assert len(reader.pages)==12
with zipfile.ZipFile(out/'遥感变化检测平台_组会汇报_20261017.pptx') as z:
    notes=[n for n in z.namelist() if n.startswith('ppt/notesSlides/notesSlide') and n.endswith('.xml')]
    assert len(notes)==12
    assert all('证据' in z.read(n).decode('utf-8') for n in notes)
print('PASS: PDF 12 pages, PPTX 12 source-cited speaker notes')
