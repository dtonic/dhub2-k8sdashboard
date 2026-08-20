# D.Hub2 Portal Color System

Complete reference for the dhub2-portal color system. This document covers all 5 layers of the color architecture, from Ant Design theme tokens down to TypeScript constants.

## Architecture Overview

```
Layer 1  Ant Design Theme         src/themes/index.tsx
         └─ colorPrimary, fontFamily for ConfigProvider

Layer 2  CSS Custom Properties     src/styles/theme.css
         └─ 100+ variables under :root / [data-theme='dark']

Layer 3  shadcn/ui HSL Variables   src/index.css (@layer base)
         └─ --background, --foreground, --primary, --secondary, --muted

Layer 4  Tailwind Config           tailwind.config.js
         └─ Maps CSS vars to utility classes (bg-primary, text-foreground)

Layer 5  TypeScript Constants      src/constants/entityColors.ts
                                   src/pages/dashboard/constants/chartColors.ts
         └─ Entity type palette, chart palettes
```

Theme switching is controlled by `src/contexts/index.tsx`:

- `data-theme` attribute (`light` / `dark`) on `<html>` for CSS variable switching
- `.dark` class on `<html>` for Tailwind dark mode
- Ant Design `algorithm` (`defaultAlgorithm` / `darkAlgorithm`)

### Ant Design surface tokens (dark)

`darkAlgorithm` derives a neutral gray container (`#141414`) that sits outside the Slate palette, so `src/themes/index.tsx` pins the dark seed tokens (ADR-0238):

| Token                               | Value     | Matches                            |
| ----------------------------------- | --------- | ---------------------------------- |
| `colorBgContainer` (dark)           | `#1e293b` | `--color-surface` (slate-800)      |
| `colorBgElevated` (dark)            | `#1e293b` | `--color-bg-elevated` (slate-800)  |
| Form controls (dark, per-component) | `#020617` | `--color-bg-container` (slate-950) |

Card, Table, fixed table cells, and popups therefore land on the same value as `--color-surface`. `Input` / `InputNumber` / `Select` / `DatePicker` / `Cascader` override it back to the field surface — a field the same color as its card stops reading as a field. Light-mode tokens are unchanged (`#ffffff` already matches).

### Product primary (fixed)

The accent primary is **fixed** for readability and consistency (light/dark tokens in `theme.css`). It is **not** user-configurable from Settings.

- Canonical hex: `#3b50ce` (`DEFAULT_PRIMARY_COLOR` in `src/utils/themeColors.ts`) — used for Ant Design `colorPrimary` in `src/contexts/index.tsx`
- CSS custom properties (`--color-primary-*`, `--primary`, accents) are defined in `src/styles/theme.css` for light and dark; runtime does not inject primary overrides from JS

The values documented below are the **product default** palette.

---

## 1. Brand Identity

| Token                    | Value                                | Usage                                                    |
| ------------------------ | ------------------------------------ | -------------------------------------------------------- |
| **Primary (Brand)**      | `#3b50ce`                            | Ant Design colorPrimary, focus rings, input focus border |
| **Primary 600**          | `#3b50ce` (light) / `#3b82f6` (dark) | Solid fills (Button, focused border), link base          |
| **Primary container**    | `#e0e7ff` (light) / `#3a44a0` (dark) | Tinted soft surface — chips, active tabs, info boxes     |
| **On primary container** | `#1e1b4b` (light) / `#c7d2fe` (dark) | Readable text/icon on primary container (WCAG AA both)   |

### Typography

| Font          | Role                           | Weight        |
| ------------- | ------------------------------ | ------------- |
| **Inter**     | Body text, labels, buttons     | 400, 500, 600 |
| **Roboto**    | Headings, section headers      | 700           |
| **Fira Code** | Code blocks, monospace content | 400           |

---

## 1.5 Container roles (MD3 pattern)

Primary has two orthogonal roles. Picking the right one is the single most important color decision in this system — mixing them up is the most common source of dark-mode contrast failures.

### Role A — Solid fill

Use when the surface IS the brand color: Button default fill, focused input border, link base color, focus rings, React Flow selected edge/handle.

| Token                  | Light     | Dark      | Pair                                |
| ---------------------- | --------- | --------- | ----------------------------------- |
| `--color-primary-600`  | `#3b50ce` | `#3b82f6` | Foreground: white / heading neutral |
| Tailwind: `bg-primary` | —         | —         | Tailwind: `text-primary-foreground` |

### Role B — Tinted surface (container)

Use for chips, tag pills, active tab/row highlights, info boxes, icon bubbles on card surfaces, hover tints on sidebar items — any surface that hints at a brand relationship without being the brand color itself.

| Token                            | Light     | Dark      | Contrast                              |
| -------------------------------- | --------- | --------- | ------------------------------------- |
| `--color-primary-container`      | `#e0e7ff` | `#3a44a0` | — (background)                        |
| `--color-on-primary-container`   | `#1e1b4b` | `#c7d2fe` | ≈ 11.7:1 (L) / 5.7:1 (D)              |
| Tailwind: `bg-primary-container` | —         | —         | Tailwind: `text-on-primary-container` |

### Do / Don't

✅ **Do**

```tsx
// Tag pill — container pair, both themes readable
<span className="bg-primary-container text-on-primary-container rounded-full px-2.5 py-0.5">
  enriched
</span>

// Info box — CSS var form
<div style={{ background: 'var(--color-primary-container)', color: 'var(--color-on-primary-container)' }}>
  ...
</div>
```

❌ **Don't**

```tsx
// Blue-on-blue in dark mode (text-primary = mid-blue, bg-primary/20 = mid-blue tint) → fails WCAG AA
<span className="bg-primary/20 text-primary rounded-full">enriched</span>

// Using `--color-primary-700` as "darker hover" — it inverts to LIGHTER indigo in dark
.my-button:hover { background: var(--color-primary-700); }  /* BROKEN in dark */

// The correct "darker on hover" in both themes:
.my-button:hover { background: color-mix(in srgb, var(--color-primary-600) 85%, black); }
```

### Why this matters

Before the container tokens existed, components reached for `bg-primary/10 + text-primary` or `--color-primary-50 + --color-primary-600`. In light mode these happened to produce acceptable contrast (indigo-dark text on indigo-tinted-white). In dark mode `--color-primary-600` is still a _mid_ blue (`#3b82f6`) — paired with any primary-tinted dark surface it becomes blue-on-blue and fails 4.5:1. The container pair fixes this by design: text and bg are chosen _together_ and verified to meet AA in both themes.

---

## 2. Semantic Color Tokens (CSS Custom Properties)

### 2.1 Text Colors

| Variable                 | Light     | Dark      | Usage                                         |
| ------------------------ | --------- | --------- | --------------------------------------------- |
| `--text-color`           | `#182026` | `#e2e8f0` | Legacy body text                              |
| `--color-heading`        | `#1f2937` | `#f1f5f9` | Headings                                      |
| `--color-text-primary`   | `#1f2937` | `#f8fafc` | Primary text                                  |
| `--color-text-secondary` | `#4b5563` | `#94a3b8` | Secondary text (AA both modes; ADR-0148)      |
| `--color-text-tertiary`  | `#6b7280` | `#8a96ad` | Tertiary text — AA on surface (ADR-0147/0148) |
| `--color-muted`          | `#64748b` | `#94a3b8` | Muted/helper text                             |
| `--color-link-fg`        | `#3b50ce` | `#a5b4fc` | Link text on NEUTRAL surfaces                 |
| `--color-link-fg-hover`  | `#192182` | `#c7d2fe` | Link text on NEUTRAL surfaces (hover)         |

> **Link tokens vs `--color-primary-600` vs `--color-on-primary-container`**
>
> - `--color-link-fg` — interactive link-like text placed on a **neutral** surface (card, panel, bg-container). Dark-mode pair is indigo-300/200 which meets WCAG AA against every neutral slate surface. Use Tailwind `text-link` / `hover:text-link-hover`, or on `<a>` tags (which already inherit these via global `a {}` styles).
> - `--color-primary-600` — **solid brand fill** (Button background, focused border, React Flow edges). Not a text token; in dark mode it is mid-blue (`#3b82f6`) and fails AA on neutral dark surfaces.
> - `--color-on-primary-container` — text placed **ON a primary-tinted container** (chip, info box). Use together with `--color-primary-container` as a pair.
>
> If you are about to write `text-primary` on a neutral card or reach for `text-[var(--color-primary-600)]` for a link, use `text-link` instead.
>
> **Branding override.** When overriding `--color-link-fg` at runtime (e.g. via `BrandingContext`), override `--color-link-fg-hover` in tandem so the hover direction (increase contrast) is preserved in both themes. Light-mode default pairs a mid-indigo base with a near-black indigo-950 hover (darker on hover); dark-mode default pairs indigo-300 with indigo-200 (lighter on hover). A branding theme that overrides only the base value can invert this direction and produce an unreadable hover state in one theme.

### 2.2 Surface & Background

| Variable                | Light     | Dark      | Usage                                                                                                  |
| ----------------------- | --------- | --------- | ------------------------------------------------------------------------------------------------------ |
| `--color-surface`       | `#ffffff` | `#1e293b` | Card/container surface                                                                                 |
| `--color-surface-muted` | `#f1f5f9` | `#1e293b` | Muted surface                                                                                          |
| `--color-panel`         | `#f3f4f6` | `#0f172a` | Panel background                                                                                       |
| `--color-bg-container`  | `#f8fafc` | `#020617` | App container background                                                                               |
| `--color-bg-elevated`   | `#ffffff` | `#1e293b` | Elevated surface (widgets)                                                                             |
| `--color-bg-subtle`     | `#f1f5f9` | `#334155` | Subtle background                                                                                      |
| `--app-chrome-bg`       | `#f6f7f8` | `#101922` | App chrome (sidebar, navbar) — must equal `LIGHT_CHROME_BG`/`DARK_CHROME_BG` in `src/themes/index.tsx` |
| `--color-page-body`     | `#ffffff` | `#101922` | Page body — the ground a list page's content sits on                                                   |

### 2.3 Border

| Variable              | Light     | Dark      | Usage           |
| --------------------- | --------- | --------- | --------------- |
| `--color-border`      | `#d4dbe7` | `#334155` | General borders |
| `--color-card-border` | `#e2e8f0` | `#334155` | Card borders    |

### 2.4 Card

| Variable                      | Light     | Dark                      |
| ----------------------------- | --------- | ------------------------- |
| `--color-card-bg`             | `#ffffff` | `#1e293b`                 |
| `--color-card-selected-bg`    | `#e6f7ff` | `rgba(59, 130, 246, 0.2)` |
| `--color-card-text`           | `#111827` | `#f8fafc`                 |
| `--color-card-text-secondary` | `#4b5563` | `#94a3b8`                 |

### 2.5 Modal

| Variable                 | Light                      | Dark                     |
| ------------------------ | -------------------------- | ------------------------ |
| `--modal-surface`        | `#ffffff`                  | `#0f172a`                |
| `--modal-surface-muted`  | `rgba(247, 249, 252, 0.9)` | `rgba(15, 23, 42, 0.9)`  |
| `--modal-border`         | `rgba(26, 35, 45, 0.08)`   | `#334155`                |
| `--modal-text-primary`   | `#1f2937`                  | `#f8fafc`                |
| `--modal-text-secondary` | `#64748b`                  | `#cbd5e1`                |
| `--modal-input-bg`       | `#f0f4f8`                  | `#1e293b`                |
| `--modal-input-text`     | `#1f2937`                  | `#e2e8f0`                |
| `--modal-footer-surface` | `rgba(247, 249, 252, 0.9)` | `rgba(15, 23, 42, 0.95)` |
| `--modal-overlay`        | `rgba(13, 17, 23, 0.6)`    | `rgba(2, 6, 23, 0.8)`    |

### 2.6 Form Input

모든 입력 primitive (`Input`, `Textarea`, `Select`) 는 이 토큰 세트를 단일 소스로 사용한다. 모달 내부에서도 동일 — 별도의 `--modal-input-*` 은 더 이상 폼 primitive 가 참조하지 않는다.

**디자인 의도**: `--input-bg` 는 `--color-bg-container` 에 매핑해 **가장 깊은 표면층** 과 같은 톤으로 맞춘다. 슬레이트-800 카드(`--color-card-bg`) 위에 올려졌을 때 입력 필드가 한 단계 가라앉은(sunken) 시각으로 읽혀, 값 입력 영역임을 분명히 드러낸다.

| Variable                  | Light                                   | Dark                                    |
| ------------------------- | --------------------------------------- | --------------------------------------- |
| `--input-bg`              | `var(--color-bg-container)` → `#f8fafc` | `var(--color-bg-container)` → `#020617` |
| `--input-border`          | `#d1d5db`                               | `#334155`                               |
| `--input-border-hover`    | `#6b7280`                               | `#475569`                               |
| `--input-text`            | `#1f2937`                               | `#f8fafc`                               |
| `--input-placeholder`     | `#6b7280`                               | `#8a96ad`                               |
| `--input-disabled-bg`     | `#f3f4f6`                               | `#0f172a`                               |
| `--input-disabled-border` | `#e5e7eb`                               | `#1e293b`                               |
| `--input-disabled-text`   | `#6b7280`                               | `#64748b`                               |
| `--input-focus-border`    | `var(--color-primary-600)`              | `var(--color-primary-600)`              |
| `--input-focus-ring`      | `rgba(59, 80, 206, 0.2)`                | `rgba(59, 80, 206, 0.3)`                |
| `--input-error-border`    | `#dc2626`                               | `#f87171`                               |
| `--input-error-ring`      | `rgba(220, 38, 38, 0.2)`                | `rgba(248, 113, 113, 0.3)`              |

`input`과 `textarea`는 hover 시 `--input-bg` 배경을 그대로 유지한다. hover 피드백은 테두리 변화로만 제공하며, 배경색을 별도로 덮어쓰지 않는다. `select`의 기존 hover 배경은 선택 컨트롤의 구분을 위해 유지한다.

Focus 규칙은 `src/styles/components/templates.css` 에서 `input.form-input:focus` 에 공통 적용:

```css
input.form-input:focus,
input.form-input:focus-visible {
  border-color: var(--color-primary-600);
  box-shadow: 0 0 0 2px var(--input-focus-ring);
}
```

### 2.7 Accent Button (Gradient)

Overridden by JS when a custom primary is selected.

| Variable                     | Light (default)                                                 | Dark (default)                                                  |
| ---------------------------- | --------------------------------------------------------------- | --------------------------------------------------------------- |
| `--color-accent-start`       | `#3b50ce`                                                       | `var(--color-primary-600)`                                      |
| `--color-accent-end`         | `#2837a8`                                                       | `var(--color-primary-600)`                                      |
| `--color-accent-hover-start` | `#2837a8`                                                       | `var(--color-primary-600)`                                      |
| `--color-accent-hover-end`   | `#192182`                                                       | `var(--color-primary-600)`                                      |
| `--color-accent-glow`        | `color-mix(in srgb, var(--color-primary-600) 30%, transparent)` | `color-mix(in srgb, var(--color-primary-600) 40%, transparent)` |

### 2.8 Login

| Variable             | Light     | Dark   |
| -------------------- | --------- | ------ |
| `--color-login-hero` | `#081c42` | (same) |

---

## 3. Status & Feedback Colors

### 3.1 Semantic Status

| Status      | Foreground (Light) | Background (Light) | Foreground (Dark) | Background (Dark) |
| ----------- | ------------------ | ------------------ | ----------------- | ----------------- |
| **Info**    | `#3b50ce`          | `#f0f5ff`          | `#60a5fa`         | `#1e3a5f`         |
| **Success** | `#16a34a`          | `#f0fdf4`          | `#4ade80`         | `#1a3a2e`         |
| **Warning** | `#f59e0b`          | `#fffbeb`          | `#fbbf24`         | `#3a2e1a`         |
| **Danger**  | `#dc2626`          | `#fef2f2`          | `#f87171`         | `#3a1a1a`         |

### 3.2 Pipeline Execution Status

| Status        | Foreground (Light) | Background (Light) | Foreground (Dark) | Background (Dark) |
| ------------- | ------------------ | ------------------ | ----------------- | ----------------- |
| **Running**   | `#3b82f6`          | `#eff6ff`          | `#60a5fa`         | `#1e3a5f`         |
| **Success**   | `#22c55e`          | `#f0fdf4`          | `#4ade80`         | `#14532d`         |
| **Failed**    | `#ef4444`          | `#fef2f2`          | `#f87171`         | `#450a0a`         |
| **Scheduled** | `#f59e0b`          | `#fffbeb`          | `#fbbf24`         | `#422006`         |
| **Pending**   | `#6b7280`          | `#f3f4f6`          | `#94a3b8`         | `#1e293b`         |

### 3.3 Diff / Version Comparison

| Status       | Foreground (Light) | Background (Light) | Foreground (Dark) | Background (Dark) |
| ------------ | ------------------ | ------------------ | ----------------- | ----------------- |
| **Added**    | `#166534`          | `#dcfce7`          | `#4ade80`         | `#14532d`         |
| **Removed**  | `#991b1b`          | `#fee2e2`          | `#f87171`         | `#450a0a`         |
| **Modified** | `#854d0e`          | `#fef3c7`          | `#fbbf24`         | `#422006`         |

### 3.4 Accent (Orange)

| Variable                     | Light     | Dark      |
| ---------------------------- | --------- | --------- |
| `--color-accent-600`         | `#f97316` | `#fb923c` |
| `--color-accent-50`          | `#fff7ed` | `#2a1e1e` |
| `--color-accent-chip-bg`     | `#ffedd5` | `#3a2418` |
| `--color-accent-text-dark`   | `#c2410c` | `#fdba74` |
| `--color-accent-border-soft` | `#fed7aa` | `#78350f` |

---

## 4. shadcn/ui HSL Variables + Tailwind Mapping

Defined in `src/index.css` under `@layer base` (base values) and `src/styles/theme.css` (runtime overrides). Consumed by `tailwind.config.js`.

Note: `theme.css` declares un-layered `:root` / `[data-theme='dark']` overrides that take precedence over the `@layer base` defaults in `index.css`. There is no runtime JS override of `--primary` for user theme picking; accent is fixed in CSS.

| CSS Variable             | Tailwind Class                | Light HSL     | Light Hex | Dark HSL      | Dark Hex  |
| ------------------------ | ----------------------------- | ------------- | --------- | ------------- | --------- |
| `--background`           | `bg-background`               | `0 0% 100%`   | `#ffffff` | `222 84% 5%`  | `#020617` |
| `--foreground`           | `text-foreground`             | `222 47% 11%` | `#0f172a` | `210 40% 98%` | `#f8fafc` |
| `--primary`              | `bg-primary` / `text-primary` | `231 60% 52%` | `#3b50ce` | `217 91% 60%` | `#3b82f6` |
| `--primary-foreground`   | `text-primary-foreground`     | `0 0% 100%`   | `#ffffff` | `222 47% 11%` | `#0f172a` |
| `--secondary`            | `bg-secondary`                | `0 0% 96.1%`  | `#f5f5f5` | `217 33% 17%` | `#1e293b` |
| `--secondary-foreground` | `text-secondary-foreground`   | `0 0% 9%`     | `#171717` | `210 40% 98%` | `#fafafa` |
| `--muted`                | `bg-muted`                    | `0 0% 96.1%`  | `#f5f5f5` | `217 33% 17%` | `#1e293b` |
| `--muted-foreground`     | `text-muted-foreground`       | `0 0% 45.1%`  | `#737373` | `215 20% 65%` | `#94a3b8` |

Additional Tailwind color classes:

| Class                 | Source                        | Value                      |
| --------------------- | ----------------------------- | -------------------------- |
| `bg-background-light` | Hardcoded                     | `#f6f7f8`                  |
| `bg-page-body`        | `var(--color-page-body)`      | `#ffffff` / `#101922`      |
| `bg-chrome`           | `var(--app-chrome-bg)`        | `#f6f7f8` / `#101922`      |
| `bg-panel`            | `var(--color-panel)`          | `#f3f4f6` / `#0f172a`      |
| `bg-modal-footer`     | `var(--modal-footer-surface)` | Modal footer surface       |
| `border-modal-border` | `var(--modal-border)`         | Modal header/footer border |
| `text-modal-muted`    | `var(--modal-text-secondary)` | Modal auxiliary text       |

Tailwind border-radius (from `--radius: 0.5rem`):

| Class           | Value                                                                                    |
| --------------- | ---------------------------------------------------------------------------------------- |
| `rounded-lg`    | `0.5rem` (8px)                                                                           |
| `rounded-md`    | `0.375rem` (6px)                                                                         |
| `rounded-sm`    | `0.25rem` (4px)                                                                          |
| `rounded-card`  | `var(--radius-lg)` = 8px                                                                 |
| `rounded-input` | `var(--radius-sm)` = 4px                                                                 |
| `rounded-node`  | `var(--radius-node)` = 12px (ontology builder only; pipeline nodes use `rounded-lg` 8px) |

---

## 5. Entity Type Colors

Each entity type has a 4-shade palette used consistently across NodeCard, CollectionTreePanel, badges, and other entity-related components.

Source: `src/constants/entityColors.ts`

### 5.1 Hex Palette

| Entity         | 100 (Light BG)        | 300 (Icon Dark)       | 600 (Icon Light)      | 900 (Dark BG)         |
| -------------- | --------------------- | --------------------- | --------------------- | --------------------- |
| **Collection** | `#e2e8f0` (slate-200) | `#cbd5e1` (slate-300) | `#475569` (slate-600) | `#0f172a` (slate-900) |
| **Dataset**    | `#E6ECF3`             | `#8DA8CC`             | `#5E7BAE`             | `#2A3A50`             |
| **Code**       | `#F2E8E0`             | `#D1A882`             | `#B87F56`             | `#4A3425`             |
| **Pipeline**   | `#E2F0EA`             | `#7DB8A2`             | `#52917A`             | `#263E34`             |
| **Document**   | `#EBE5F2`             | `#A893C7`             | `#7E63A5`             | `#322546`             |
| **Knowledge**  | `#ECE5F3`             | `#AD96CC`             | `#876BAF`             | `#352849`             |
| **Default**    | `#f3f4f6` (gray-100)  | `#d1d5db` (gray-300)  | `#4b5563` (gray-600)  | `#374151` (gray-700)  |

### 5.2 Tailwind Class Map

Light mode icon/text classes:

| Entity     | Icon Text        | Badge BG       | Badge Text       |
| ---------- | ---------------- | -------------- | ---------------- |
| Collection | `text-slate-600` | `bg-slate-200` | `text-slate-700` |
| Dataset    | `text-[#5E7BAE]` | `bg-[#E6ECF3]` | `text-[#3D5A82]` |
| Code       | `text-[#B87F56]` | `bg-[#F2E8E0]` | `text-[#8B5E38]` |
| Pipeline   | `text-[#52917A]` | `bg-[#E2F0EA]` | `text-[#3D6D5C]` |
| Document   | `text-[#7E63A5]` | `bg-[#EBE5F2]` | `text-[#5E4880]` |
| Knowledge  | `text-[#876BAF]` | `bg-[#ECE5F3]` | `text-[#6B4F91]` |

Dark mode icon/text classes:

| Entity     | Icon Text        | Badge BG       | Badge Text       |
| ---------- | ---------------- | -------------- | ---------------- |
| Collection | `text-slate-300` | `bg-slate-700` | `text-slate-300` |
| Dataset    | `text-[#8DA8CC]` | `bg-[#2A3A50]` | `text-[#8DA8CC]` |
| Code       | `text-[#C4946A]` | `bg-[#4A3425]` | `text-[#D1A882]` |
| Pipeline   | `text-[#7DB8A2]` | `bg-[#263E34]` | `text-[#7DB8A2]` |
| Document   | `text-[#A893C7]` | `bg-[#322546]` | `text-[#A893C7]` |
| Knowledge  | `text-[#AD96CC]` | `bg-[#352849]` | `text-[#AD96CC]` |

---

## 6. Chart Colors

Source: `src/pages/dashboard/constants/chartColors.ts`

### 6.1 Categorical Palette (10 colors)

Databricks-style muted/desaturated palette for enterprise dashboard aesthetic.

| Index | Name         | RGBA                        |
| ----- | ------------ | --------------------------- |
| 0     | Muted Blue   | `rgba(102, 136, 187, 0.85)` |
| 1     | Muted Teal   | `rgba(85, 170, 170, 0.85)`  |
| 2     | Muted Amber  | `rgba(221, 170, 68, 0.85)`  |
| 3     | Muted Coral  | `rgba(217, 123, 123, 0.85)` |
| 4     | Muted Green  | `rgba(119, 187, 136, 0.85)` |
| 5     | Muted Slate  | `rgba(136, 153, 170, 0.85)` |
| 6     | Muted Pink   | `rgba(204, 136, 153, 0.85)` |
| 7     | Light Blue   | `rgba(153, 187, 221, 0.85)` |
| 8     | Muted Orange | `rgba(232, 166, 69, 0.85)`  |
| 9     | Gray         | `rgba(184, 184, 184, 0.85)` |

### 6.2 Sequential Scales (Heatmaps)

**Blue Scale:**
| Shade | Hex |
|-------|-----|
| Light | `#B8C8DD` |
| Medium | `#7799BB` |
| Dark | `#445577` |

**Teal Scale:**
| Shade | Hex |
|-------|-----|
| Light | `#B8DDDD` |
| Medium | `#66AAAA` |
| Dark | `#446666` |

### 6.3 Diverging Scales

**Teal-Coral:**
| Position | Hex |
|----------|-----|
| Low | `#55AAAA` |
| Neutral | `#B8B8B8` |
| High | `#D97B7B` |

**Blue-Amber:**
| Position | Hex |
|----------|-----|
| Low | `#6688BB` |
| Neutral | `#B8B8B8` |
| High | `#DDAA44` |

### 6.4 Dashboard Theme Colors

| Token                 | Light     | Dark      |
| --------------------- | --------- | --------- |
| `background.base`     | `#ffffff` | `#0d1117` |
| `background.elevated` | `#f8fafc` | `#161b22` |
| `background.surface`  | `#f1f5f9` | `#21262d` |
| `border`              | `#e2e8f0` | `#30363d` |
| `text.primary`        | `#1e293b` | `#f8fafc` |
| `text.secondary`      | `#64748b` | `#94a3b8` |
| `text.muted`          | `#64748b` | `#64748b` |

---

## 7. Canvas (React Flow)

| Variable                | Light                    | Dark      |
| ----------------------- | ------------------------ | --------- |
| `--color-canvas-bg`     | `#f8fafc`                | `#0f172a` |
| `--color-canvas-border` | `rgba(15, 23, 42, 0.08)` | `#334155` |
| `--color-canvas-stroke` | `#e5e7eb`                | `#334155` |
| `--color-canvas-grid`   | `#e9ecf0`                | `#1a2436` |
| `--color-canvas-dot`    | `#cbd5e1`                | `#253350` |

---

## 8. Icon / Logo Brand Colors

| Variable                          | Light     | Dark      |
| --------------------------------- | --------- | --------- |
| `--color-logo-python-blue-1`      | `#327ebd` | —         |
| `--color-logo-python-blue-2`      | `#1565a7` | —         |
| `--color-logo-python-yellow-1`    | `#ffda4b` | —         |
| `--color-logo-python-yellow-2`    | `#f9c600` | —         |
| `--color-logo-sql-disk`           | `#ffda44` | —         |
| `--color-logo-sql-disk-alt`       | `#ffe873` | —         |
| `--color-logo-kafka-stroke`       | `#1a1919` | —         |
| `--color-logo-kafka-stroke-light` | `#ffffff` | —         |
| `--color-logo-deltalake`          | `#00afd6` | —         |
| `--color-logo-rest`               | `#4285f4` | `#60a5fa` |
| `--color-logo-ontology`           | `#000000` | —         |
| `--color-logo-ontology-invert`    | `#ffffff` | —         |

---

## 9. Spacing, Layout, Radius, Shadow & Z-Index

### Spacing

| Token         | Value  |
| ------------- | ------ |
| `--space-2xs` | `2px`  |
| `--space-xs`  | `4px`  |
| `--space-sm`  | `8px`  |
| `--space-md`  | `12px` |
| `--space-lg`  | `16px` |
| `--space-xl`  | `24px` |
| `--space-2xl` | `32px` |
| `--space-3xl` | `48px` |
| `--space-4xl` | `64px` |
| `--space-5xl` | `96px` |

### Layout Dimensions

| Token                        | Value    |
| ---------------------------- | -------- |
| `--layout-navbar-height`     | `50px`   |
| `--layout-sidebar-width`     | `240px`  |
| `--layout-sidebar-collapsed` | `60px`   |
| `--layout-content-max-w`     | `1400px` |
| `--layout-panel-width`       | `320px`  |

### Radius

| Token                 | Value                                                            |
| --------------------- | ---------------------------------------------------------------- |
| `--radius-sm`         | `4px`                                                            |
| `--radius-md`         | `6px`                                                            |
| `--radius-lg`         | `8px`                                                            |
| `--radius-node`       | `12px` (ontology builder only; pipeline nodes use `--radius-lg`) |
| `--radius` (Tailwind) | `0.5rem` (8px)                                                   |

### Shadow

| Token         | Value                                  |
| ------------- | -------------------------------------- |
| `--shadow-sm` | `0 1px 2px rgba(15, 23, 42, 0.06)`     |
| `--shadow-md` | `0 8px 20px rgba(15, 23, 42, 0.12)`    |
| `--shadow-lg` | `0 18px 36px rgba(148, 163, 184, 0.2)` |

### Z-Index

| Token         | Value  |
| ------------- | ------ |
| `--z-base`    | `1`    |
| `--z-overlay` | `1000` |
| `--z-modal`   | `1100` |
| `--z-popover` | `1200` |

---

## 10. Dark Mode Palette Reference

Dark mode is built on the Tailwind Slate palette:

| Name      | Hex       | Usage                                                            |
| --------- | --------- | ---------------------------------------------------------------- |
| slate-950 | `#020617` | `--color-bg-container` (deepest layer)                           |
| slate-900 | `#0f172a` | `--color-panel`, `--modal-surface`, canvas bg                    |
| slate-800 | `#1e293b` | `--color-surface`, card bg, input bg                             |
| slate-700 | `#334155` | `--color-border`, canvas stroke                                  |
| slate-600 | `#475569` | Input border hover                                               |
| slate-500 | `#64748b` | Input disabled text (placeholder/tertiary → `#8a96ad`, ADR-0147) |
| slate-400 | `#94a3b8` | `--color-muted`, secondary text                                  |
| slate-300 | `#cbd5e1` | Modal secondary text                                             |
| slate-200 | `#e2e8f0` | `--text-color`                                                   |
| slate-100 | `#f1f5f9` | `--color-heading`                                                |
| slate-50  | `#f8fafc` | `--color-text-primary`, card text                                |

---

## 11. Typography Scale

### Font Size

| Token              | Value  |
| ------------------ | ------ |
| `--font-size-xs`   | `11px` |
| `--font-size-sm`   | `12px` |
| `--font-size-md`   | `13px` |
| `--font-size-base` | `14px` |
| `--font-size-lg`   | `16px` |
| `--font-size-xl`   | `18px` |
| `--font-size-2xl`  | `20px` |
| `--font-size-3xl`  | `24px` |
| `--font-size-4xl`  | `30px` |

### Line Height

| Token                   | Value  |
| ----------------------- | ------ |
| `--line-height-tight`   | `1.2`  |
| `--line-height-snug`    | `1.35` |
| `--line-height-normal`  | `1.5`  |
| `--line-height-relaxed` | `1.7`  |

### Letter Spacing

| Token               | Value     |
| ------------------- | --------- |
| `--tracking-tight`  | `-0.02em` |
| `--tracking-snug`   | `-0.01em` |
| `--tracking-normal` | `0em`     |
| `--tracking-wide`   | `0.05em`  |

---

## 12. Motion & Timing

| Token                | Value                               |
| -------------------- | ----------------------------------- |
| `--duration-instant` | `50ms`                              |
| `--duration-fast`    | `150ms`                             |
| `--duration-normal`  | `250ms`                             |
| `--duration-slow`    | `400ms`                             |
| `--duration-enter`   | `300ms`                             |
| `--duration-exit`    | `200ms`                             |
| `--ease-in`          | `cubic-bezier(0.4, 0, 1, 1)`        |
| `--ease-out`         | `cubic-bezier(0, 0, 0.2, 1)`        |
| `--ease-in-out`      | `cubic-bezier(0.4, 0, 0.2, 1)`      |
| `--ease-spring`      | `cubic-bezier(0.34, 1.56, 0.64, 1)` |

---

## 13. Workflow Node Category Colors

Used by Agent Workflow Builder nodes. Each category has a stroke/bg/text triple.

| Category        | Stroke (Light) | BG (Light) | Text (Light) | Stroke (Dark) | BG (Dark)                  | Text (Dark) |
| --------------- | -------------- | ---------- | ------------ | ------------- | -------------------------- | ----------- |
| **Flow**        | `#6366f1`      | `#eef2ff`  | `#3730a3`    | `#818cf8`     | `rgba(99, 102, 241, 0.15)` | `#c7d2fe`   |
| **AI**          | `#8b5cf6`      | `#f5f3ff`  | `#5b21b6`    | `#a78bfa`     | `rgba(139, 92, 246, 0.15)` | `#ddd6fe`   |
| **Data**        | `#0ea5e9`      | `#f0f9ff`  | `#0369a1`    | `#38bdf8`     | `rgba(14, 165, 233, 0.15)` | `#bae6fd`   |
| **Integration** | `#f59e0b`      | `#fffbeb`  | `#92400e`    | `#fbbf24`     | `rgba(245, 158, 11, 0.15)` | `#fde68a`   |
| **Control**     | `#10b981`      | `#ecfdf5`  | `#065f46`    | `#34d399`     | `rgba(16, 185, 129, 0.15)` | `#a7f3d0`   |
| **Advanced**    | `#ef4444`      | `#fef2f2`  | `#991b1b`    | `#f87171`     | `rgba(239, 68, 68, 0.15)`  | `#fecaca`   |

Boolean edge colors: `--color-wf-true` = emerald-500/400, `--color-wf-false` = red-500/400.

---

## 14. Icon Sizing

Scoped to `svg.tabler-icon` to avoid affecting decorative SVGs.

| Token            | Value  | Context                  |
| ---------------- | ------ | ------------------------ |
| `--icon-size-xs` | `12px` | Alerts, notifications    |
| `--icon-size-sm` | `14px` | Input prefixes           |
| `--icon-size-md` | `16px` | Buttons, navbar          |
| `--icon-size-lg` | `18px` | Sidebar, main navbar     |
| `--icon-size-xl` | `24px` | AI assistant drawer icon |

---

## Source Files

| File                                           | Purpose                                                         |
| ---------------------------------------------- | --------------------------------------------------------------- |
| `src/themes/index.tsx`                         | Ant Design font tokens and dark-mode component overrides        |
| `src/styles/theme.css`                         | 100+ CSS custom properties (light/dark)                         |
| `src/index.css`                                | shadcn/ui HSL variables (`@layer base`)                         |
| `tailwind.config.js`                           | CSS variable to Tailwind class mapping                          |
| `src/constants/entityColors.ts`                | Entity type color palette                                       |
| `src/pages/dashboard/constants/chartColors.ts` | Chart palettes                                                  |
| `components.json`                              | shadcn/ui config (new-york style, neutral base)                 |
| `src/contexts/index.tsx`                       | Theme switching (mode, locale); fixed `colorPrimary`            |
| `src/contexts/BrandingContext.tsx`             | Branding API (logos, titles); not primary color injection       |
| `src/utils/themeColors.ts`                     | `DEFAULT_PRIMARY_COLOR`, `generateCSSVariables` (tests/tooling) |

---

## Visual Reference

Open in a browser to view an interactive design token catalog:

| File                                   | Description                                       |
| -------------------------------------- | ------------------------------------------------- |
| [DESIGN.md](DESIGN.md)                 | Complete design system documentation (9 sections) |
| [preview.html](preview.html)           | Interactive design token catalog (light)          |
| [preview-dark.html](preview-dark.html) | Interactive design token catalog (dark)           |
