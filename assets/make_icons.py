#!/usr/bin/env python3
"""Regenerates the PNG app icons from the same geometry as assets/icon.svg (needs Pillow).

    python3 assets/make_icons.py

Writes transparent-cornered PNGs to internal/web/ (icon.png 256, icon-512.png) and desktop/icon.png.
"""
import os, sys
from PIL import Image, ImageDraw

S = 16                      # supersampling: draw at 16x, then shrink
N = 64 * S

def lerp(a, b, t): return tuple(int(a[i] + (b[i] - a[i]) * t) for i in range(3))
def hexc(h): return tuple(int(h[i:i+2], 16) for i in (1, 3, 5))

def vgrad(y0, y1, c0, c1):
    g = Image.new("RGB", (N, N))
    px = g.load(); a, b = hexc(c0), hexc(c1)
    for y in range(N):
        t = min(1, max(0, (y / S - y0) / (y1 - y0)))
        col = lerp(a, b, t)
        for x in range(N): px[x, y] = col
    return g

def arc_mask(cx, cy, r, w):
    m = Image.new("L", (N, N), 0); d = ImageDraw.Draw(m)
    R = (r + w / 2) * S
    d.arc([(cx * S - R), (cy * S - R), (cx * S + R), (cy * S + R)], 0, 180, fill=255, width=int(w * S))
    for ex in (cx - r, cx + r):                      # round caps
        d.ellipse([ex * S - w / 2 * S, cy * S - w / 2 * S, ex * S + w / 2 * S, cy * S + w / 2 * S], fill=255)
    return m

def gem_mask():
    m = Image.new("L", (N, N), 0); d = ImageDraw.Draw(m)
    pts = [(32, 14.5), (39.5, 22), (32, 29.5), (24.5, 22)]
    d.polygon([(x * S, y * S) for x, y in pts], fill=255)
    d.line([(x * S, y * S) for x, y in pts + [pts[0], pts[1]]], fill=255, width=int(2.2 * S), joint="curve")
    return m

def render(size, tile=True):
    img = Image.new("RGBA", (N, N), (0, 0, 0, 0))
    if tile:
        bg = Image.new("RGBA", (N, N), (5, 8, 12, 255))
        # soft blue glow behind the mark
        glow = Image.new("RGBA", (N, N), (0, 0, 0, 0)); gd = ImageDraw.Draw(glow)
        for i in range(30, 0, -1):
            a = int(46 * (1 - i / 30) ** 2)
            r = i * 1.1 * S
            gd.ellipse([32 * S - r, 26 * S - r, 32 * S + r, 26 * S + r], fill=(26, 111, 176, a))
        bg = Image.alpha_composite(bg, glow)
        tm = Image.new("L", (N, N), 0)
        ImageDraw.Draw(tm).rounded_rectangle([0, 0, N - 1, N - 1], radius=15 * S, fill=255)
        img.paste(bg, (0, 0), tm)
        ring = Image.new("L", (N, N), 0); rd = ImageDraw.Draw(ring)
        rd.rounded_rectangle([0, 0, N - 1, N - 1], radius=15 * S, outline=255, width=S)
        img.paste(Image.new("RGBA", (N, N), (26, 42, 58, 255)), (0, 0), ring)
    dy = 2 if tile else 0
    layers = [(arc_mask(32, 30 + dy, 22, 4.4), vgrad(30 + dy, 53 + dy, "#5ec1ff", "#1f6fff")),
              (arc_mask(32, 30 + dy, 15, 4.4), vgrad(30 + dy, 46 + dy, "#8fdcff", "#3b95ff")),
              (arc_mask(32, 30 + dy, 8, 4.4), vgrad(30 + dy, 39 + dy, "#d9f4ff", "#6dc8ff"))]
    gm = gem_mask()
    if dy: gm = Image.eval(gm.transform(gm.size, Image.AFFINE, (1, 0, 0, 0, 1, -dy * S)), lambda v: v)
    layers.append((gm, vgrad(14 + dy, 30 + dy, "#ffffff", "#9fdcff")))
    for m, g in layers: img.paste(g.convert("RGBA"), (0, 0), m)
    return img.resize((size, size), Image.LANCZOS)

if __name__ == "__main__":
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
    out = {"internal/web/icon.png": 256, "internal/web/icon-512.png": 512, "desktop/icon.png": 256}
    for rel, size in out.items():
        render(size).save(os.path.join(root, rel)); print("wrote", rel, size)
    render(1024).save(os.path.join(root, "assets", "icon-1024.png")); print("wrote assets/icon-1024.png")
