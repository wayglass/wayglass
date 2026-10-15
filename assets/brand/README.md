# Wayglass icons

`wayglass-w.png` is the original project W artwork. `app-icon.svg` embeds that
image unchanged, clips its background to an 824 px rounded tile on a 1024 px
canvas, and adds a shadow and rim. The transparent margin is 100 px. The desktop
PNG is rendered from this SVG; Wails makes the platform icon during its build.

Web favicons use the same artwork at 32, 180 and 512 px. `favicon.svg` embeds
the 180 px PNG, preserving the original mark without approximating its outline.
The SVG is a raster wrapper, not a vector trace.

These assets and the icon composition are covered by the project's MIT license.
