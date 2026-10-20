"""Stage 1: refine the Vision mask, decontaminate edges, split off the pedestal.
Usage: python3 cut.py <design_dir>   -> work/cut-full.png, work/cut-nopedestal.png (original res)"""
import sys, numpy as np
from PIL import Image
from scipy import ndimage as ndi
D = sys.argv[1]
C = np.asarray(Image.open(D+'/mascot-zefirca-original.jpeg').convert('RGB')).astype(np.float32)/255
m = np.asarray(Image.open(D+'/work/work-cut-mask-all.png').convert('L')).astype(np.float32)/255
# tighten the soft edge a little (Vision masks bleed ~1px of background)
a = np.clip((m-0.15)/0.75, 0, 1)
a = a*a*(3-2*a)
# background estimate: normalised blur of pixels that are certainly background
w = (m < 0.02).astype(np.float32)
def nblur(x, s): return ndi.gaussian_filter(x, s)
den = nblur(w, 20)+1e-6
B = np.stack([nblur(C[..., i]*w, 20)/den for i in range(3)], -1)
# un-mix: C = a F + (1-a) B  ->  F
edge = (a > 0.02) & (a < 0.98)
F = C.copy()
F[edge] = np.clip((C[edge]-(1-a[edge, None])*B[edge])/a[edge, None], 0, 1)
a[a <= 0.02] = 0
def save(F, a, path):
    rgba = np.dstack([F, a])
    Image.fromarray((rgba*255+0.5).astype(np.uint8)).save(path)
save(F, a, D+'/work/cut-full.png')
# pedestal: geometric cylinder (top ellipse cy=654, bottom cy=716, cx=712, a=293, b=36,
# measured from the mask), then colour-classify inside it so the feet survive.
H, W = a.shape
yy, xx = np.mgrid[0:H, 0:W].astype(np.float32)
cx, ax, bx = 712.0, 296.0, 38.0
def ell(cy): return ((xx-cx)/ax)**2+((yy-cy)/bx)**2 <= 1
region = ell(654) | ell(716) | ((yy >= 654) & (yy <= 716) & (np.abs(xx-cx) <= ax))
R, G, Bc = C[..., 0], C[..., 1], C[..., 2]
minty = (G > R+0.01) & (G >= Bc-0.01)
lum = 0.3*R+0.59*G+0.11*Bc
ped = region & minty & ((lum > 0.66) | (yy >= 666))
ped |= region & (G > Bc+0.035) & (G > R+0.06)   # mint shadow between the feet
ped |= region & (yy >= 674)          # nothing of the character is below the soles
ped = ndi.binary_opening(~ped, iterations=1) == 0
a2 = a*(1-ped)
a2 = ndi.grey_opening(a2, size=(3, 3))   # drop 1-2px slivers of the rim highlight
sm = ndi.gaussian_filter(a2, 0.8)
a2 = np.where(region, np.minimum(a2, sm*1.2), a2)
# keep only the largest connected component (the character)
lab, n = ndi.label(a2 > 0.3)
if n:
    sizes = ndi.sum(np.ones_like(a2), lab, range(1, n+1))
    keep = lab == (1+int(np.argmax(sizes)))
    keep = ndi.binary_dilation(keep, iterations=2)
    a2 = a2*keep
# soften the new cut edge
a2 = np.minimum(a2, ndi.gaussian_filter(a2, 0.7)*1.15)
save(F, np.clip(a2, 0, 1), D+'/work/cut-nopedestal.png')
print('ok', Image.open(D+'/work/cut-nopedestal.png').getbbox())
