"""Stage 3: render assets/preview.png, a contact sheet for review.

Usage: python3 preview.py <design_dir>
"""
import os
import sys

import numpy as np
from PIL import Image, ImageDraw, ImageFont

D = sys.argv[1]
A = os.path.join(D, 'assets')
FONT = '/System/Library/Fonts/Helvetica.ttc'
f_h = ImageFont.truetype(FONT, 40)
f_s = ImageFont.truetype(FONT, 22)


def img(*p):
    return Image.open(os.path.join(A, *p)).convert('RGBA')


def superellipse(size, n=5.0):
    s = size * 4
    yy, xx = np.mgrid[0:s, 0:s].astype(np.float32)
    u = np.abs((xx + 0.5) / s * 2 - 1)
    v = np.abs((yy + 0.5) / s * 2 - 1)
    m = (u ** n + v ** n) <= 1
    return Image.fromarray((m * 255).astype(np.uint8)).resize((size, size), Image.LANCZOS)


def circle(size):
    s = size * 4
    yy, xx = np.mgrid[0:s, 0:s].astype(np.float32)
    m = np.hypot(xx - s / 2 + 0.5, yy - s / 2 + 0.5) <= s / 2
    return Image.fromarray((m * 255).astype(np.uint8)).resize((size, size), Image.LANCZOS)


def rounded(size, r):
    m = Image.new('L', (size * 4, size * 4), 0)
    ImageDraw.Draw(m).rounded_rectangle((0, 0, size * 4 - 1, size * 4 - 1), r * 4, fill=255)
    return m.resize((size, size), Image.LANCZOS)


def masked(src, size, mask):
    t = src.resize((size, size), Image.LANCZOS)
    t.putalpha(Image.fromarray((np.asarray(t.getchannel('A')).astype(np.float32) * np.asarray(mask) / 255).astype(np.uint8)))
    return t


def wallpaper(w, h, dark):
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float32)
    if dark:
        a, b = np.array([28, 24, 48]), np.array([10, 30, 40])
    else:
        a, b = np.array([250, 214, 230]), np.array([196, 232, 240])
    t = (xx / w * 0.6 + yy / h * 0.4)[..., None]
    rgb = a * (1 - t) + b * t
    return Image.fromarray(rgb.astype(np.uint8)).convert('RGBA')


def android_view(fg, bg, size, mask):
    """Adaptive icon as a launcher shows it: the central 72dp of the 108dp canvas, masked."""
    c = bg.copy()
    c.alpha_composite(fg)
    inset = int(1024 * 18 / 108)
    c = c.crop((inset, inset, 1024 - inset, 1024 - inset))
    return masked(c, size, mask(size))


W = 2400
sheet = Image.new('RGBA', (W, 4200), (246, 245, 248, 255))
dr = ImageDraw.Draw(sheet)
y = 40
dr.text((60, y), 'Agent Zefirca - asset contact sheet', font=ImageFont.truetype(FONT, 56), fill=(30, 28, 40))
y += 100

# ---- iOS icon sizes on light and dark wallpapers ----------------------------------
ios_light = img('icons/icon.png')
_ic = os.path.join(D, 'work', 'ic', 'Dark.png')
ios_dark = Image.open(_ic).convert('RGBA') if os.path.exists(_ic) else ios_light
sizes = [180, 120, 87, 60, 58, 40, 29]
for label, icon, dark in (('iOS default icon on light wallpaper', ios_light, False),
                          ('iOS dark appearance on dark wallpaper', ios_dark, True)):
    dr.text((60, y), label, font=f_h, fill=(30, 28, 40))
    y += 60
    band = wallpaper(W - 120, 420, dark)
    big = masked(icon, 380, superellipse(380))
    band.alpha_composite(big, (20, 20))
    x = 440
    bd = ImageDraw.Draw(band)
    for s in sizes:
        band.alpha_composite(masked(icon, s, superellipse(s)), (x, 210 - s // 2))
        bd.text((x, 340), f'{s}px', font=f_s, fill=(240, 240, 250) if dark else (40, 40, 60))
        x += s + 70
    bd.text((20, 405 - 22), '1024 (shown at 380)', font=f_s, fill=(240, 240, 250) if dark else (40, 40, 60))
    sheet.alpha_composite(band, (60, y))
    y += 460

# ---- iOS appearances ------------------------------------------------------------
dr.text((60, y), 'iOS appearances (Icon Composer render via ictool)', font=f_h, fill=(30, 28, 40))
y += 60
x = 60
renders = []
icdir = os.path.join(D, 'work', 'ic')
for n, lab in (('Default', 'default / light'), ('Dark', 'default / dark'), ('TintedDark', 'default / tinted'),
               ('ClearLight', 'default / clear')):
    p = os.path.join(icdir, n + '.png')
    if os.path.exists(p):
        renders.append((Image.open(p).convert('RGBA'), lab))
for im, lab in renders:
    sheet.alpha_composite(im.resize((300, 300), Image.LANCZOS), (x, y))
    dr.text((x, y + 310), lab, font=f_s, fill=(40, 40, 60))
    x += 370
y += 380

# ---- Android adaptive ---------------------------------------------------------------
dr.text((60, y), 'Android adaptive icon: circle / squircle / rounded square, themed (monochrome)', font=f_h, fill=(30, 28, 40))
y += 60
fg = img('icons/android-icon-foreground.png')
bg = img('icons/android-icon-background.png')
mono = img('icons/android-icon-monochrome.png')
band = wallpaper(W - 120, 330, False)
x = 20
for f, b, mk, lab in ((fg, bg, circle, 'circle'), (fg, bg, lambda s: superellipse(s, 4), 'squircle'),
                      (fg, bg, lambda s: rounded(s, s // 4), 'rounded')):
    band.alpha_composite(android_view(f, b, 220, mk), (x, 20))
    band.alpha_composite(android_view(f, b, 48, mk), (x + 240, 100))
    ImageDraw.Draw(band).text((x, 260), lab + ' 220 / 48px', font=f_s, fill=(40, 40, 60))
    x += 340
for tb, tf, lab in (((211, 227, 253), (4, 30, 73), 'themed light'), ((0, 74, 119), (194, 231, 255), 'themed dark')):
    tbg = Image.new('RGBA', (1024, 1024), tb + (255,))
    tfg = Image.new('RGBA', (1024, 1024), tf + (255,))
    tfg.putalpha(mono.getchannel('A'))
    band.alpha_composite(android_view(tfg, tbg, 220, circle), (x, 20))
    band.alpha_composite(android_view(tfg, tbg, 48, circle), (x + 240, 100))
    ImageDraw.Draw(band).text((x, 260), lab, font=f_s, fill=(40, 40, 60))
    x += 340
sheet.alpha_composite(band, (60, y))
y += 370

# ---- avatars -----------------------------------------------------------------------
dr.text((60, y), 'Assistant avatar at 32 / 40 / 56 pt (@3x and @1x) on light and dark chat surfaces', font=f_h, fill=(30, 28, 40))
y += 60
head = img('mascot/avatar-head.png')
for dark in (False, True):
    surf = Image.new('RGBA', (W - 120, 220), (24, 22, 32, 255) if dark else (255, 255, 255, 255))
    sd = ImageDraw.Draw(surf)
    x = 20
    for pt in (32, 40, 56):
        for scale in (3, 1):
            s = pt * scale
            disc = Image.new('RGBA', (s, s), (74, 40, 60, 255) if dark else (250, 228, 238, 255))
            disc.alpha_composite(head.resize((s, s), Image.LANCZOS))
            surf.alpha_composite(masked(disc, s, circle(s)), (x, 110 - s // 2))
            sd.text((x, 185), f'{pt}pt@{scale}x', font=f_s, fill=(220, 220, 230) if dark else (60, 60, 70))
            x += max(s, 90) + 40
    x += 40
    for pt in (40, 56):
        s = pt * 3
        surf.alpha_composite(head.resize((s, s), Image.LANCZOS), (x, 110 - s // 2))
        sd.text((x, 185), f'transparent {pt}pt@3x', font=f_s, fill=(220, 220, 230) if dark else (60, 60, 70))
        x += s + 120
    sheet.alpha_composite(surf, (60, y))
    y += 240

# ---- splash + full body ------------------------------------------------------------
y += 10
dr.text((60, y), 'Splash (imageWidth 200) light / dark, full-body mascot (with and without pedestal)', font=f_h, fill=(30, 28, 40))
y += 60
splash = img('splash/splash-mascot.png')
x = 60
for bgc, lab in (((248, 241, 248), 'splash light #F8F1F8'), ((22, 19, 29), 'splash dark #16131D')):
    ph = Image.new('RGBA', (390, 844), bgc + (255,))
    s = 200 * 390 // 390
    sp = splash.resize((s, s), Image.LANCZOS)
    ph.alpha_composite(sp, ((390 - s) // 2, (844 - s) // 2))
    mk = Image.new('L', (390, 844), 0)
    ImageDraw.Draw(mk).rounded_rectangle((0, 0, 389, 843), 56, fill=255)
    ph.putalpha(mk)
    sheet.alpha_composite(ph, (x, y))
    dr.text((x, y + 860), lab, font=f_s, fill=(40, 40, 60))
    x += 440
for name, dark in (('mascot/mascot-full.png', False), ('mascot/mascot-full.png', True), ('mascot/mascot-full-pedestal.png', False)):
    m = img(name)
    box = Image.new('RGBA', (400, 844), (24, 22, 32, 255) if dark else (236, 232, 244, 255))
    r = min(380 / m.width, 820 / m.height)
    mm = m.resize((int(m.width * r), int(m.height * r)), Image.LANCZOS)
    box.alpha_composite(mm, ((400 - mm.width) // 2, (844 - mm.height) // 2))
    sheet.alpha_composite(box, (x, y))
    dr.text((x, y + 860), name.split('/')[-1] + (' on dark' if dark else ''), font=f_s, fill=(40, 40, 60))
    x += 440
y += 910

sheet = sheet.crop((0, 0, W, y + 20))
sheet.convert('RGB').save(os.path.join(A, 'preview.png'), optimize=True)
print('preview', sheet.size)
