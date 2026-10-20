"""Stage 2: build every Agent Zefirca asset from the stage-1 cut-outs.

Usage: python3 build.py <design_dir>
Inputs : <design_dir>/work/cut-full.png, <design_dir>/work/cut-nopedestal.png  (from cut.py)
Outputs: <design_dir>/assets/**  (see README.md)
"""
import os
import sys

import numpy as np
from PIL import Image, ImageFilter

D = sys.argv[1]
OUT = os.path.join(D, 'assets')
FULL = Image.open(os.path.join(D, 'work/cut-full.png')).convert('RGBA')
NOPED = Image.open(os.path.join(D, 'work/cut-nopedestal.png')).convert('RGBA')

# ---- palette (sampled from the mascot render's pastel backdrop, tuned) -------------
PAL = {
    # SaaS default icon backdrop: pink top -> lavender -> mint bottom
    'light': ['#FCE4F1', '#EDE4FA', '#D5F2E9'],
    'dark': ['#3A2A4F', '#241D36', '#12202A'],
    'splash_light': '#F8F1F8',
    'splash_dark': '#16131D',
}


def hex2rgb(h):
    h = h.lstrip('#')
    return np.array([int(h[i:i + 2], 16) for i in (0, 2, 4)], np.float32) / 255


def out(*p):
    path = os.path.join(OUT, *p)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    return path


def scaled(img, box, scale, sharpen=True):
    """Crop `box` (original px) and resample by `scale` (premultiplied Lanczos)."""
    c = img.crop(box)
    w, h = c.size
    r = c.resize((max(1, round(w * scale)), max(1, round(h * scale))), Image.LANCZOS)
    if sharpen and scale > 1:
        rgb = r.convert('RGB').filter(ImageFilter.UnsharpMask(radius=1.4 * scale / 2, percent=45, threshold=1))
        rgb.putalpha(r.getchannel('A'))
        r = rgb
    return r


def fade_bottom(img, frac=0.22):
    """Soft alpha fade over the bottom `frac` of the image, so a bust has no hard cut."""
    a = np.asarray(img.getchannel('A')).astype(np.float32)
    h = a.shape[0]
    n = int(h * frac)
    ramp = np.ones(h, np.float32)
    t = np.linspace(0, 1, n)
    ramp[h - n:] = 1 - t * t * (3 - 2 * t)
    a = a * ramp[:, None]
    o = img.copy()
    o.putalpha(Image.fromarray(a.astype(np.uint8)))
    return o


def gradient(size, stops, angle_deg=100):
    """Linear gradient across `stops` (hex list), angle measured from +x, clockwise-down."""
    w, h = size
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float32)
    th = np.deg2rad(angle_deg)
    d = np.array([np.cos(th), np.sin(th)])
    p = (xx - w / 2) * d[0] + (yy - h / 2) * d[1]
    p = (p - p.min()) / (p.max() - p.min())
    cols = [hex2rgb(s) for s in stops]
    seg = len(cols) - 1
    idx = np.minimum((p * seg).astype(int), seg - 1)
    t = p * seg - idx
    t = t * t * (3 - 2 * t)
    c0 = np.stack(cols)[idx]
    c1 = np.stack(cols)[idx + 1]
    rgb = c0 + (c1 - c0) * t[..., None]
    # soft top-left highlight so it reads as glass rather than flat paint
    r = np.sqrt(((xx - w * 0.3) / w) ** 2 + ((yy - h * 0.2) / h) ** 2)
    rgb = rgb + (np.clip(1 - r / 0.55, 0, 1) ** 2 * 0.06)[..., None]
    # tiny dither, kills banding in 8-bit exports
    rgb = rgb + (np.random.default_rng(7).random(rgb.shape) - 0.5) / 255
    return Image.fromarray((np.clip(rgb, 0, 1) * 255 + 0.5).astype(np.uint8))


def place(canvas_size, layer, x, y):
    c = Image.new('RGBA', canvas_size, (0, 0, 0, 0))
    c.alpha_composite(layer, (int(x), int(y)))
    return c


def contact_shadow(size, cx, cy, rx, ry, opacity):
    w, h = size
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float32)
    d = ((xx - cx) / rx) ** 2 + ((yy - cy) / ry) ** 2
    a = np.clip(1 - d, 0, 1) ** 1.6 * opacity
    img = Image.new('RGBA', size, (40, 30, 60, 0))
    img.putalpha(Image.fromarray((a * 255).astype(np.uint8)))
    return img


def flatten(bg, fg):
    o = bg.convert('RGBA')
    o.alpha_composite(fg)
    return o.convert('RGB')


def save_opaque(img, path):
    img.convert('RGB').save(path, optimize=True)


# ---- crops in ORIGINAL pixel coordinates (1408x768 render) -------------------------
BUST = (420, 30, 960, 666)       # head, antenna, hologram hand, coat ... down to the soles
HEAD = (498, 8, 928, 438)        # square, face-centred, for avatars (hologram masked out)
CHAR_BOX = (427, 38, 953, 666)   # no-pedestal bbox
FULL_BOX = (409, 35, 1016, 757)  # with-pedestal bbox

# ---- 1. mascot cut-outs ------------------------------------------------------------
pad = 24
for name, img, box in [('mascot-full-pedestal', FULL, FULL_BOX), ('mascot-full', NOPED, CHAR_BOX)]:
    b = (box[0] - pad, box[1] - pad, box[2] + pad, box[3] + pad)
    for sfx, s in [('', 3.0), ('@1x', 1.0)]:
        scaled(img, b, s).save(out('mascot', f'{name}{sfx}.png'), optimize=True)

bust = fade_bottom(scaled(NOPED, (420, 28, 960, 470), 3.0), 0.28)
bust.save(out('mascot', 'mascot-bust.png'), optimize=True)

_noholo = NOPED.copy()
_na = np.asarray(_noholo.getchannel('A')).copy()
_na[230:420, :536] = 0          # drop the hologram hand so the avatar is just the character
_noholo.putalpha(Image.fromarray(_na))
head = scaled(_noholo, HEAD, 1024 / (HEAD[2] - HEAD[0]))
head_faded = fade_bottom(head, 0.2)
head_faded.save(out('mascot', 'avatar-head.png'), optimize=True)


# ---- 2. the icon mark: head and collar only ------------------------------------------
# Every icon and the splash use one centred head-only mark, never the full-body or bust
# cut: launcher masks, the iOS squircle and 29 pt sizes all need a simple centred shape.
# MARK_C is the mark's centre and MARK_R its radius in original pixels: the head (with
# antenna) sits inside a hard circle above the centre line, the collar fades out below it,
# so nothing of the mark is drawn outside MARK_R. `reach` = MARK_R after scaling, as a
# fraction of the canvas, so each output states its safe zone directly.
MARK_C = (712, 205)
MARK_R = 200


def _smooth(t):
    t = np.clip(t, 0, 1)
    return t * t * (3 - 2 * t)


def _mark_source():
    a = np.asarray(_noholo.getchannel('A')).astype(np.float32)
    H, W = a.shape
    yy, xx = np.mgrid[0:H, 0:W].astype(np.float32)
    cx, cy = MARK_C
    r = np.hypot(xx - cx, yy - cy)
    hard = np.clip(MARK_R - r + 0.5, 0, 1)
    fade = _smooth((MARK_R - r) / (0.30 * MARK_R))
    below = _smooth((yy - (cy + 0.05 * MARK_R)) / (0.30 * MARK_R))
    m = hard * (1 - below) + fade * below
    o = _noholo.copy()
    o.putalpha(Image.fromarray((a * m).astype(np.uint8)))
    return o


MARK = _mark_source()


def mark(reach, canvas=1024):
    """The colour mark on a transparent `canvas`, all content within `reach`*canvas of the centre."""
    cx, cy = MARK_C
    box = (cx - MARK_R, cy - MARK_R, cx + MARK_R, cy + MARK_R)
    s = reach * canvas / MARK_R
    layer = scaled(MARK, box, s)
    return place((canvas, canvas), layer, canvas / 2 - layer.width / 2, canvas / 2 - layer.height / 2)


def glyph(reach, canvas=1024):
    """Flat single-colour glyph for themed icons: the swirl-head outline with the eyes,
    brows, smile and two swirl grooves cut out. White on transparent; only alpha matters."""
    from scipy import ndimage as ndi
    im = np.asarray(NOPED).astype(np.float32) / 255
    r, g, b, a = im[..., 0], im[..., 1], im[..., 2], im[..., 3]
    lum = 0.3 * r + 0.59 * g + 0.11 * b
    H, W = a.shape
    yy, xx = np.mgrid[0:H, 0:W]

    def largest(m):
        lab, n = ndi.label(m)
        if not n:
            return m
        return lab == 1 + int(np.argmax(ndi.sum(m, lab, range(1, n + 1))))

    # head = opaque, not the blue collar/headset, above the shirt; chin rounded by an ellipse
    head = (yy < 340) & (xx > 530) & (xx < 900) & (a > 0.5) & ((b - r) <= 0.06)
    head = ndi.binary_fill_holes(largest(head))
    head &= (yy < 190) | (((xx - 712) / 168) ** 2 + ((yy - 190) / 142) ** 2 <= 1)
    head = ndi.binary_fill_holes(largest(ndi.binary_opening(head, iterations=6)))
    head = ndi.gaussian_filter(head.astype(np.float32), 4) > 0.5
    # features: the dark eyes, brows and mouth inside the face box, closed and filled
    feat = head & (lum < 0.45) & (xx > 600) & (xx < 810) & (yy > 150) & (yy < 290)
    feat = ndi.binary_fill_holes(ndi.binary_closing(feat, iterations=3))
    lab, n = ndi.label(feat)
    sizes = ndi.sum(feat, lab, range(1, n + 1))
    feat = ndi.binary_dilation(np.isin(lab, [i + 1 for i, s in enumerate(sizes) if s > 120]), iterations=3)

    def groove(cx, cy, rx, ry, rot, a0, a1):
        m = np.zeros_like(head)
        t = np.deg2rad(np.linspace(a0, a1, 800))
        ro = np.deg2rad(rot)
        x0, y0 = rx * np.cos(t), ry * np.sin(t)
        px = (cx + x0 * np.cos(ro) - y0 * np.sin(ro)).astype(int)
        py = (cy + x0 * np.sin(ro) + y0 * np.cos(ro)).astype(int)
        m[py, px] = True
        return ndi.binary_dilation(m, iterations=4)

    grooves = groove(720, 175, 175, 70, -18, 205, 330) | groove(735, 105, 110, 42, -22, 200, 335)
    gl = ndi.gaussian_filter((head & ~feat & ~grooves).astype(np.float32), 2.0)
    gl = np.clip((gl - 0.5) * 4 + 0.5, 0, 1)
    ys, xs = np.nonzero(gl > 0.06)
    gcx, gcy = (xs.min() + xs.max()) / 2, (ys.min() + ys.max()) / 2
    rad = np.hypot(xs - gcx, ys - gcy).max()
    src = Image.new('RGBA', (W, H), (255, 255, 255, 0))
    src.putalpha(Image.fromarray((gl * 255).astype(np.uint8)))
    box = (int(gcx - rad), int(gcy - rad), int(gcx + rad) + 1, int(gcy + rad) + 1)
    layer = scaled(src, box, reach * canvas / rad, sharpen=False)
    return place((canvas, canvas), layer, canvas / 2 - layer.width / 2, canvas / 2 - layer.height / 2)


def fitted(img, reach, canvas=1024):
    """`img` (any RGBA art) scaled and centred so every visible pixel is within `reach`."""
    al = np.asarray(img.getchannel('A'))
    ys, xs = np.nonzero(al > 8)
    cx, cy = (xs.min() + xs.max()) / 2, (ys.min() + ys.max()) / 2
    rad = np.hypot(xs - cx, ys - cy).max()
    s = reach * canvas / rad
    layer = img.resize((max(1, round(img.width * s)), max(1, round(img.height * s))), Image.LANCZOS)
    return place((canvas, canvas), layer, canvas / 2 - cx * s, canvas / 2 - cy * s)


# Reaches (fraction of the canvas, measured from its centre):
#  iOS: the system squircle has ~0.225*canvas corner radius; Apple's grid keeps the
#       primary shape in the central ~80%, so 0.40 keeps the mark clear of every corner.
#  Android adaptive: 108 dp canvas, 66 dp safe circle = 0.306; 0.29 leaves a margin.
#  Themed (monochrome): smaller than the colour mark, Material's themed glyphs sit in ~48 dp.
#  Splash (Android 12+): the icon is masked to a circle of radius 1/3; 0.30 leaves a margin.
IOS_REACH, ANDROID_REACH, MONO_REACH, SPLASH_REACH = 0.40, 0.29, 0.25, 0.30

ios_fg = mark(IOS_REACH)
and_fg = mark(ANDROID_REACH)

# iOS / Expo fallback / store source: opaque gradient + mark
save_opaque(flatten(gradient((1024, 1024), PAL['light']), ios_fg), out('icons', 'icon.png'))
# Icon Composer layer: the mark alone, the document's gradient fill is the background
ios_fg.save(out('icons', 'zefirca.icon', 'Assets', 'zefirca.png'), optimize=True)
# Android adaptive
and_fg.save(out('icons', 'android-icon-foreground.png'), optimize=True)
save_opaque(gradient((1024, 1024), PAL['light']), out('icons', 'android-icon-background.png'))
glyph(MONO_REACH).save(out('icons', 'android-icon-monochrome.png'), optimize=True)
# web favicon (48px, the flattened icon)
flatten(gradient((1024, 1024), PAL['light']), ios_fg).resize((48, 48), Image.LANCZOS).save(
    out('icons', 'favicon.png'), optimize=True)

# ---- 3. splash ---------------------------------------------------------------------
# The bust with the hologram (the in-app identity), fitted inside the Android 12+ circle.
sp = 1024
splash = fitted(fade_bottom(scaled(NOPED, (420, 28, 960, 520), 1.6), 0.26), SPLASH_REACH, sp)
splash.save(out('splash', 'splash-mascot.png'), optimize=True)
for mode, bgc in (('light', PAL['splash_light']), ('dark', PAL['splash_dark'])):
    bg = Image.new('RGBA', (sp, sp), tuple(int(x * 255) for x in hex2rgb(bgc)) + (255,))
    bg.alpha_composite(splash)
    bg.convert('RGB').save(out('splash', f'splash-preview-{mode}.png'), optimize=True)

print('built into', OUT)

# ---- 5. Icon Composer documents (.icon) ---------------------------------------------
import json


def p3(h):
    r, g, b = hex2rgb(h)
    return f'srgb:{r:.5f},{g:.5f},{b:.5f},1.00000'


def icon_doc(light, dark):
    def grad(stops):
        return {'linear-gradient': [p3(stops[0]), p3(stops[-1])],
                'orientation': {'start': {'x': 0.5, 'y': 0.0}, 'stop': {'x': 0.5, 'y': 1.0}}}
    return {
        'fill-specializations': [
            {'value': grad(light)},
            {'appearance': 'dark', 'value': grad(dark)},
        ],
        'groups': [{
            'layers': [{
                'image-name': 'zefirca.png',
                'name': 'zefirca',
                'glass': False,
            }],
            'shadow': {'kind': 'neutral', 'opacity': 0.35},
            'translucency': {'enabled': False, 'value': 0.0},
            'specular': False,
        }],
        'supported-platforms': {'squares': 'shared'},
    }


with open(out('icons', 'zefirca.icon', 'icon.json'), 'w') as f:
    json.dump(icon_doc(PAL['light'], PAL['dark']), f, indent=2)
print('icon document written')
