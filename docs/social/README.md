# Social assets

Palette (apple.com style): white `#ffffff` / `#f5f5f7`, ink `#1d1d1f`, secondary `#6e6e73`,
hairline `#d2d2d7`, blue `#0071e3` to `#2997ff`. Dark variant: `#000` / `#0b0b0f`, text `#f5f5f7`,
cards `#1d1d1f`, blue `#2997ff`. Orange (`#ff6a2a`) appears exactly once per image, as a small
accent dot — never as a background
or primary color. The Zyvor "Z" mark (`zyvor-mark.svg`) is drawn in the same blue gradient.

| File | What it is | Rebuild |
|---|---|---|
| `kairon-share-card.png` | 1200×630 card: the README Open Graph preview | `./docs/social/build-social-card.sh` |
| `kairon-social-card.html` / `.jpg` | 1600×900 (16:9) card for LinkedIn and X: the product story in five steps | same script |
| `kairon-share-card.html` | Source for the 1200×630 share card | same script |
| `readme/capabilities.html` → `../assets/readme-capabilities.jpg` | 1600×640 Run / Move / Protect / Automate | same script |
| `readme/architecture.html` → `../assets/readme-architecture.jpg` | 1600×700 components and ports | same script |
| `../assets/social-preview.svg` / `.png` | 1280×640 README hero, light | `python3 docs/social/build-social-svg.py docs/assets` then `rsvg-convert -w 1280 -h 640 docs/assets/social-preview.svg -o docs/assets/social-preview.png` |
| `../assets/social-preview-dark.svg` / `.png` | Same hero, dark (used via `prefers-color-scheme: dark` in the README) | same, with `social-preview-dark` |

Needs Google Chrome and macOS `sips` for the three HTML-sourced cards (already on a Mac; nothing
to install), and `rsvg-convert` (librsvg, e.g. `brew install librsvg`) for the SVG-sourced hero
pair. `build-social-svg.py` holds the light and dark palettes and generates both hero SVGs from one
layout, so they cannot drift apart.

Every claim on the social cards is already sourced in the project README and docs.
Licence wording follows `LICENSE`: Zyvor Production License v1.0.
