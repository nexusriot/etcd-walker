#!/usr/bin/env python3
"""Render a `tmux capture-pane -e -p` dump (ANSI SGR colours) to a PNG that
looks like a terminal window. Usage: ansi2png.py in.ansi out.png"""
import re, sys, unicodedata
from PIL import Image, ImageDraw, ImageFont

MONO = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
MONO_B = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"
SANS = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
EMOJI = "/usr/share/fonts/truetype/noto/NotoColorEmoji.ttf"
SIZE = 15
BG = (24, 25, 33)
FG = (204, 208, 214)
PAD = 18
TITLEBAR = 30

# xterm 16-colour palette (a slightly softened one)
BASE16 = [BG, (220, 80, 80), (80, 200, 120), (230, 200, 90), (90, 140, 230), (190, 110, 210),
          (80, 200, 210), (204, 208, 214), (100, 104, 118), (255, 110, 110), (120, 230, 150),
          (250, 225, 120), (130, 170, 255), (220, 150, 240), (120, 230, 240), (250, 250, 250)]


def xterm256(n):
    if n < 16:
        return BASE16[n]
    if n < 232:
        n -= 16
        steps = [0, 95, 135, 175, 215, 255]
        return (steps[n // 36], steps[(n // 6) % 6], steps[n % 6])
    v = 8 + (n - 232) * 10
    return (v, v, v)


# box-drawing: (up, down, left, right), 1 = single, 2 = double
BOX = {
    "─": (0, 0, 1, 1), "│": (1, 1, 0, 0), "┌": (0, 1, 0, 1), "┐": (0, 1, 1, 0),
    "└": (1, 0, 0, 1), "┘": (1, 0, 1, 0), "├": (1, 1, 0, 1), "┤": (1, 1, 1, 0),
    "┬": (0, 1, 1, 1), "┴": (1, 0, 1, 1), "┼": (1, 1, 1, 1),
    "═": (0, 0, 2, 2), "║": (2, 2, 0, 0), "╔": (0, 2, 0, 2), "╗": (0, 2, 2, 0),
    "╚": (2, 0, 0, 2), "╝": (2, 0, 2, 0),
}


def draw_box(d, c, px, py, cw, ch, col):
    up, down, left, right = BOX[c]
    cx, cy = px + cw // 2, py + ch // 2
    def seg(kind, x0, y0, x1, y1, vertical):
        offs = [0] if kind == 1 else [-2, 2]
        for o in offs:
            if vertical:
                d.line([x0 + o, y0, x1 + o, y1], fill=col, width=1)
            else:
                d.line([x0, y0 + o, x1, y1 + o], fill=col, width=1)
    if up: seg(up, cx, py, cx, cy, True)
    if down: seg(down, cx, cy, cx, py + ch, True)
    if left: seg(left, px, cy, cx, cy, False)
    if right: seg(right, cx, cy, px + cw, cy, False)


def parse(text):
    """-> list of rows; each row a list of (char, fg, bg, bold, reverse)."""
    rows = []
    for line in text.rstrip("\n").split("\n"):
        cells = []
        fg, bg, bold, rev = None, None, False, False
        i = 0
        while i < len(line):
            m = re.match(r"\x1b\[([0-9;]*)m", line[i:])
            if m:
                params = [int(p) if p else 0 for p in m.group(1).split(";")] if m.group(1) else [0]
                j = 0
                while j < len(params):
                    p = params[j]
                    if p == 0: fg, bg, bold, rev = None, None, False, False
                    elif p == 1: bold = True
                    elif p == 22: bold = False
                    elif p == 7: rev = True
                    elif p == 27: rev = False
                    elif 30 <= p <= 37: fg = BASE16[p - 30]
                    elif 90 <= p <= 97: fg = BASE16[p - 90 + 8]
                    elif 40 <= p <= 47: bg = BASE16[p - 40]
                    elif 100 <= p <= 107: bg = BASE16[p - 100 + 8]
                    elif p == 39: fg = None
                    elif p == 49: bg = None
                    elif p in (38, 48) and j + 1 < len(params):
                        if params[j + 1] == 5:
                            col = xterm256(params[j + 2]); j += 2
                        else:
                            col = tuple(params[j + 2:j + 5]); j += 4
                        if p == 38: fg = col
                        else: bg = col
                    j += 1
                i += m.end()
                continue
            ch = line[i]
            cells.append((ch, fg, bg, bold, rev))
            if unicodedata.east_asian_width(ch) in "WF":
                cells.append(("", fg, bg, bold, rev))  # wide char's second cell
            i += 1
        rows.append(cells)
    return rows


def main(src, dst):
    rows = parse(open(src, encoding="utf-8").read())
    mono = ImageFont.truetype(MONO, SIZE)
    mono_b = ImageFont.truetype(MONO_B, SIZE)
    sans = ImageFont.truetype(SANS, SIZE)
    emoji = ImageFont.truetype(EMOJI, 109)
    cw = round(mono.getlength("M"))
    ch = SIZE + 5
    cols = max(len(r) for r in rows)
    W, H = cols * cw + 2 * PAD, len(rows) * ch + 2 * PAD + TITLEBAR
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    # window chrome
    d.rectangle([0, 0, W, TITLEBAR], fill=(36, 38, 48))
    for k, col in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
        d.ellipse([14 + k * 22, 9, 26 + k * 22, 21], fill=col)
    for y, row in enumerate(rows):
        for x, (c, fg, bg, bold, rev) in enumerate(row):
            fgc, bgc = fg or FG, bg
            if rev:
                fgc, bgc = (bg or BG), (fg or FG)
            px, py = PAD + x * cw, TITLEBAR + PAD + y * ch
            wide = unicodedata.east_asian_width(c) in "WF" if c else False
            if bgc:
                d.rectangle([px, py, px + cw * (2 if wide else 1) - 1, py + ch - 1], fill=bgc)
            if not c or c == " ":
                continue
            if c in BOX:
                draw_box(d, c, px, py, cw, ch, fgc)
                continue
            if wide and ord(c) > 0x1F000:
                g = Image.new("RGBA", (136, 128), (0, 0, 0, 0))
                ImageDraw.Draw(g).text((0, 0), c, font=emoji, embedded_color=True)
                g = g.crop(g.getbbox() or (0, 0, 1, 1)).resize((ch - 4, ch - 4), Image.LANCZOS)
                img.paste(g, (px + 1, py + 2), g)
                continue
            font = mono_b if bold else mono
            if font.getmask(c).getbbox() is None and c.strip():
                font = sans
            d.text((px, py + 1), c, font=font, fill=fgc)
    img.save(dst, optimize=True)


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
