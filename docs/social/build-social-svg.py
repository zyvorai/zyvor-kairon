# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
import sys  # usage: python3 docs/social/build-social-svg.py docs/assets
LIGHT = dict(
    bg0="#ffffff", bg1="#f5f5f7", wash="#0071e3", wash_op="0.10", wash2_op="0.05",
    ink="#1d1d1f", sec="#6e6e73", card="#ffffff", card_stroke="#d2d2d7", shadow_op="0.10",
    blue0="#0071e3", blue1="#2997ff", link="#0071e3",
    chip_fill="#e8f2fd", chip_text="#0066cc", chip_stroke="#b9d7f7",
    wire="#0071e3", hi_fill="#e8f2fd", hi_stroke="#0071e3", hi_text="#0066cc",
    hair="#e5e5ea")
DARK = dict(
    bg0="#000000", bg1="#0b0b0f", wash="#2997ff", wash_op="0.20", wash2_op="0.08",
    ink="#f5f5f7", sec="#a1a1a6", card="#1d1d1f", card_stroke="#3a3a3c", shadow_op="0.55",
    blue0="#0a84ff", blue1="#5eb0ff", link="#2997ff",
    chip_fill="#0b2542", chip_text="#66b2ff", chip_stroke="#17426f",
    wire="#2997ff", hi_fill="#0b2542", hi_stroke="#2997ff", hi_text="#66b2ff",
    hair="#2c2c2e")

def svg(p):
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="640" viewBox="0 0 1280 640" role="img" aria-label="Kairon — Real VMs on Kubernetes no pod per VM">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1280" y2="640" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="{p['bg0']}"/>
      <stop offset="1" stop-color="{p['bg1']}"/>
    </linearGradient>
    <linearGradient id="mark" x1="0" y1="0" x2="120" y2="120" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="{p['blue0']}"/>
      <stop offset="1" stop-color="{p['blue1']}"/>
    </linearGradient>
    <radialGradient id="glow" cx="200" cy="200" r="420" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="{p['wash']}" stop-opacity="{p['wash_op']}"/>
      <stop offset="1" stop-color="{p['wash']}" stop-opacity="0"/>
    </radialGradient>
    <linearGradient id="hair" x1="96" y1="0" x2="560" y2="0" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="{p['blue0']}"/>
      <stop offset="1" stop-color="{p['blue0']}" stop-opacity="0"/>
    </linearGradient>
  </defs>

  <rect width="1280" height="640" fill="url(#bg)"/>
  <rect width="1280" height="640" fill="url(#glow)"/>

  <!-- faint grid -->
  <g stroke="{p['ink']}" stroke-opacity="0.035" stroke-width="1">
    <path d="M0 160 H1280 M0 320 H1280 M0 480 H1280"/>
    <path d="M320 0 V640 M640 0 V640 M960 0 V640"/>
  </g>

  <!-- brand mark -->
  <rect x="96" y="112" width="104" height="104" rx="24" fill="url(#mark)"/>
  <path d="M126 136 176 136 126 196 176 196" fill="none" stroke="#ffffff"
        stroke-width="13" stroke-linecap="round" stroke-linejoin="round"/>

  <!-- wordmark -->
  <text x="96" y="318" font-family="'SF Pro Display','Helvetica Neue',Arial,sans-serif"
        font-size="108" font-weight="700" fill="{p['ink']}" letter-spacing="-2">Kairon</text>

  <rect x="96" y="342" width="220" height="3" fill="url(#hair)"/>

  <!-- tagline -->
  <text x="96" y="392" font-family="'SF Pro Text','Helvetica Neue',Arial,sans-serif"
        font-size="28" font-weight="400" fill="{p['sec']}">Real VMs on Kubernetes — no pod per VM</text>

  <!-- chips: one row -->
  <g font-family="'SF Mono','Menlo',monospace" font-size="18" font-weight="600" fill="{p['chip_text']}">
    <rect x="96" y="448" width="228" height="44" rx="22" fill="{p['chip_fill']}" stroke="{p['chip_stroke']}"/>
    <text x="120" y="476">No virt-launcher</text>

    <rect x="340" y="448" width="168" height="44" rx="22" fill="{p['chip_fill']}" stroke="{p['chip_stroke']}"/>
    <text x="364" y="476">Go stdlib</text>

    <rect x="524" y="448" width="168" height="44" rx="22" fill="{p['chip_fill']}" stroke="{p['chip_stroke']}"/>
    <text x="548" y="476">Live migrate</text>

    <rect x="708" y="448" width="196" height="44" rx="22" fill="{p['chip_fill']}" stroke="{p['chip_stroke']}"/>
    <text x="732" y="476">NeedsRecovery</text>
  </g>

  <text x="96" y="560" font-family="'SF Mono','Menlo',monospace" font-size="16" fill="{p['sec']}">Zyvor Production License · FluxVM · KVM</text>

  <!-- architecture motif -->
  <g>
    <rect x="860" y="120" width="300" height="72" rx="14" fill="{p['card']}" stroke="{p['card_stroke']}"/>
    <text x="1010" y="152" text-anchor="middle" font-family="'SF Mono','Menlo',monospace" font-size="15" font-weight="600" fill="{p['ink']}">Kubernetes API</text>
    <text x="1010" y="174" text-anchor="middle" font-family="'SF Mono','Menlo',monospace" font-size="12" fill="{p['sec']}">Machine · Migration · Snapshot</text>

    <path d="M1010 192 V226" stroke="{p['wire']}" stroke-opacity="0.55" stroke-width="2" fill="none"/>
    <path d="M910 226 H1110" stroke="{p['wire']}" stroke-opacity="0.55" stroke-width="2" fill="none"/>
    <path d="M910 226 V252 M1110 226 V252" stroke="{p['wire']}" stroke-opacity="0.55" stroke-width="2" fill="none"/>

    <rect x="820" y="252" width="180" height="56" rx="12" fill="{p['card']}" stroke="{p['card_stroke']}"/>
    <text x="910" y="286" text-anchor="middle" font-family="'SF Mono','Menlo',monospace" font-size="14" font-weight="600" fill="{p['ink']}">kairon-controller</text>

    <rect x="1020" y="252" width="180" height="56" rx="12" fill="{p['card']}" stroke="{p['card_stroke']}"/>
    <text x="1110" y="286" text-anchor="middle" font-family="'SF Mono','Menlo',monospace" font-size="14" font-weight="600" fill="{p['ink']}">kairon-node</text>

    <path d="M1110 308 V340" stroke="{p['wire']}" stroke-opacity="0.55" stroke-width="2" fill="none"/>
    <rect x="1000" y="340" width="220" height="48" rx="12" fill="{p['hi_fill']}" stroke="{p['hi_stroke']}" stroke-opacity="0.6"/>
    <text x="1110" y="370" text-anchor="middle" font-family="'SF Mono','Menlo',monospace" font-size="14" font-weight="600" fill="{p['hi_text']}">FluxVM · KVM</text>
    <circle cx="1210" cy="350" r="5" fill="#ff6a2a"/>
  </g>

  <text x="1184" y="560" text-anchor="end" font-family="'SF Mono','Menlo',monospace" font-size="15" fill="{p['sec']}">github.com/zyvorai/kairon</text>
  <text x="1184" y="592" text-anchor="end" font-family="'SF Pro Text','Helvetica Neue',Arial,sans-serif"
        font-size="22" font-weight="600" fill="{p['link']}">zyvor.dev</text>
</svg>
'''
out=sys.argv[1]
open(f"{out}/social-preview.svg","w").write(svg(LIGHT))
open(f"{out}/social-preview-dark.svg","w").write(svg(DARK))
