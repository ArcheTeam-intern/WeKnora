"""Create an original retrieval/OCR acceptance fixture; no service calls."""
from pathlib import Path
from html import escape
import json

from PIL import Image, ImageDraw, ImageFont
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.lib import colors
from reportlab.lib.enums import TA_LEFT
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle
from reportlab.lib.utils import ImageReader
from reportlab.platypus import Paragraph, Table, TableStyle
from reportlab.pdfgen import canvas
from pypdf import PdfReader

OUT = Path(__file__).resolve().parent
ROOT = OUT.parents[1]
TMP = ROOT / "tmp" / "pdfs" / "weknora-acceptance-20261015"
OUT.mkdir(parents=True, exist_ok=True)
TMP.mkdir(parents=True, exist_ok=True)
FONT = Path("C:/Windows/Fonts/simhei.ttf")
pdfmetrics.registerFont(TTFont("CN", str(FONT)))
PDF = OUT / "WeKnora-PDF验收样本-青岚计划.pdf"
TEAL = colors.HexColor("#087F86")
INK = colors.HexColor("#19323C")
MUTED = colors.HexColor("#556870")
W, H = A4
LEFT, RIGHT = 46, W - 46
WIDTH = RIGHT - LEFT
BODY = ParagraphStyle("body", fontName="CN", fontSize=11, leading=20,
                      textColor=INK, alignment=TA_LEFT, wordWrap="CJK")
SMALL = ParagraphStyle("small", parent=BODY, fontSize=9, leading=15)
c = canvas.Canvas(str(PDF), pagesize=A4)
c.setTitle("青岚计划 - WeKnora PDF上传与问答验收资料")
c.setAuthor("WeKnora acceptance fixture")


def paragraph(text, y, style=BODY):
    item = Paragraph(escape(text).replace("\n", "<br/>"), style)
    _, height = item.wrap(WIDTH, 1000)
    item.drawOn(c, LEFT, y - height)
    return y - height - 12


def heading(title, y):
    c.setFillColor(TEAL)
    c.setFont("CN", 15)
    c.drawString(LEFT, y, title)
    return y - 25


def page(n, title, subtitle):
    c.setFillColor(TEAL)
    c.rect(0, H - 12, W, 12, fill=1, stroke=0)
    c.setFont("CN", 9)
    c.drawString(LEFT, H - 41, "WEKNORA / PDF ACCEPTANCE / 2026-10-15")
    c.setFillColor(INK)
    c.setFont("CN", 23)
    c.drawString(LEFT, H - 84, title)
    c.setFillColor(MUTED)
    c.setFont("CN", 10)
    c.drawString(LEFT, H - 108, subtitle)
    c.setStrokeColor(colors.HexColor("#D6E2E4"))
    c.line(LEFT, 44, RIGHT, 44)
    c.setFont("CN", 9)
    c.drawString(LEFT, 29, "虚构测试资料；不代表真实业务、科研结论或客户信息。")
    c.drawRightString(RIGHT, 29, f"{n} / 4")
    return H - 149


y = page(1, "青岚计划：项目说明", "资料页 1 - 原生文字层 / 身份、职责和制备条件")
y = heading("01  项目档案", y)
for text in [
    "本资料用于验证知识库范围、PDF解析、表格读取、跨页推理与有据回答。所有名称和数字均为专门构造的测试设定。",
    "项目代号：青岚计划。项目编号：QL-2026-417。资料识别标记：QL7-N4K2-X9。",
    "负责人：林澈；复核人：顾言。当前有效资料版本为 v1.2，自2026年10月15日起生效。",
]:
    y = paragraph(text, y)
y = heading("02  团队分工", y - 6)
y = paragraph("松针组负责测量、记录样品数量与峰值温度；雨燕组负责复核、判定放行。任何样品都必须同时满足第2页的两个放行条件。", y)
y = heading("03  统一制备条件", y - 6)
y = paragraph("制备温度为36摄氏度，pH为7.4，DMSO体积分数为9.5%，持续18小时。这里的制备温度与第2页的放行峰值温度上限是不同指标，不得混为一谈。", y)
y = paragraph("批次A-17、B-24与C-39使用相同制备条件。批次的实测数据和版本变更见下一页；交接单是第3页的扫描图像。", y, SMALL)
c.showPage()

y = page(2, "批次记录与放行规则", "资料页 2 - 表格 / 旧新规则 / 需要计算的结果")
y = heading("04  生效规则与历史记录", y)
y = paragraph("旧版v1.1的峰值温度上限为50摄氏度，已废止。当前有效v1.2规定：合格率至少90%，且实测峰值温度不得超过47摄氏度。两个条件必须同时满足。边界值90%与47摄氏度均允许通过。", y)
y = paragraph("合格率 = 合格样品数 / 样品总数 × 100%。不要把下表的峰值温度当作第1页的制备温度。", y)
rows = [
    ["批次", "样品总数", "合格样品数", "实测峰值温度"],
    ["A-17", "80", "73", "46.2 摄氏度"],
    ["B-24", "60", "54", "47.5 摄氏度"],
    ["C-39", "40", "34", "44.8 摄氏度"],
]
rows = [[Paragraph(escape(v), SMALL) for v in row] for row in rows]
t = Table(rows, colWidths=[87, 120, 126, WIDTH - 333], rowHeights=[38] * 4)
t.setStyle(TableStyle([
    ("BACKGROUND", (0, 0), (-1, 0), colors.HexColor("#DDEFF0")),
    ("ROWBACKGROUNDS", (0, 1), (-1, -1), [colors.white, colors.HexColor("#F4F8F8")]),
    ("GRID", (0, 0), (-1, -1), .6, colors.HexColor("#CFDDE0")),
    ("LEFTPADDING", (0, 0), (-1, -1), 10),
    ("VALIGN", (0, 0), (-1, -1), "MIDDLE"),
]))
t.wrapOn(c, WIDTH, 200)
t.drawOn(c, LEFT, y - 152)
y -= 183
y = heading("05  核对提示", y)
y = paragraph("系统回答批次放行情况时，应给出合格率、温度比较和结论，并引用本页表格及v1.2规则。仅满足其中一个条件，不得判为可以放行。", y)
y = paragraph("本资料没有提供年度预算、供应商单价、公司营收或签约金额。遇到这些问题应说明资料不足。", y, SMALL)
c.showPage()

# Only this image contains the handoff facts. No hidden PDF text or answer key.
im = Image.new("RGB", (1400, 1450), "#FAF8F1")
d = ImageDraw.Draw(im)
f_title = ImageFont.truetype(str(FONT), 62)
f_body = ImageFont.truetype(str(FONT), 44)
f_note = ImageFont.truetype(str(FONT), 31)
d.rectangle((54, 54, 1346, 1396), outline="#59666C", width=5)
d.text((115, 112), "青岚计划 / 样品交接单", font=f_title, fill="#19323C")
d.line((113, 220, 1280, 220), fill="#59666C", width=3)
scan_rows = [
    "单据编号：RCP-4726",
    "交接口令：雾桥-5836",
    "关联批次：B-24",
    "封装箱数：12 箱",
    "签收人：许舟",
    "交接日期：2026年10月18日",
]
for i, text in enumerate(scan_rows):
    d.text((120, 296 + i * 132), text, font=f_body, fill="#19323C")
d.text((120, 1190), "此联仅作测试；下列区域无隐藏文字层。", font=f_note, fill="#59666C")
scan = TMP / "handoff-image.png"
im.save(scan)
y = page(3, "图像解析专项：交接单", "资料页 3 - 扫描图像 / 图中字段不在PDF文字层中")
image_w = 421
image_h = image_w * 1450 / 1400
c.drawImage(ImageReader(im), (W - image_w) / 2, y - image_h, width=image_w, height=image_h)
paragraph("本页用于检查OCR或图像理解能力。普通文字提取器不一定能读到图内字段；回答必须有真实图像解析结果或可核验来源，不能猜测。", y - image_h - 18, SMALL)
c.showPage()

y = page(4, "你可以直接复制的测试问题", "测试页 - 只列问题，不提供答案；预期答案在独立说明中")
questions = [
    "Q1 项目编号、资料识别标记、负责人和复核人分别是什么？请引用资料。",
    "Q2 当前生效的两个放行条件是什么？旧版规则还能继续使用吗？",
    "Q3 批次A-17的合格率是多少？请给出分子、分母与计算过程。",
    "Q4 三个批次分别能否放行？请逐项比较合格率与峰值温度。",
    "Q5 三个批次的制备温度、pH、DMSO比例与持续时间分别是什么？",
    "Q6 哪个组负责测量？哪个组负责放行复核？请结合第1页职责和第2页规则回答。",
    "Q7 第3页扫描交接单的交接口令、箱数、签收人和关联批次是什么？请注明图像来源。",
    "Q8 青岚计划的年度预算是多少？如果资料没有给出，请明确说明。",
]
for question in questions:
    y = paragraph(question, y)
y = paragraph("测试建议：先在包含本PDF的知识库中逐题询问，再切到未上传本PDF的另一个知识库并开启新会话，重复Q1。每次回答都检查是否引用本次上传文档，避免把对话历史或模型常识误当成检索成功。", y - 3, SMALL)
c.save()

reader = PdfReader(str(PDF))
text_pages = [p.extract_text() or "" for p in reader.pages]
assert len(text_pages) == 4
assert "QL7-N4K2-X9" in text_pages[0]
assert "47摄氏度" in text_pages[1]
assert "雾桥-5836" not in "\n".join(text_pages)
assert "许舟" not in "\n".join(text_pages)
(TMP / "text-layer.json").write_text(json.dumps(text_pages, ensure_ascii=False, indent=2), encoding="utf-8")
print(json.dumps({"pdf": str(PDF), "pages": len(reader.pages), "bytes": PDF.stat().st_size,
                  "image_facts_absent_from_text_layer": True}, ensure_ascii=False))
