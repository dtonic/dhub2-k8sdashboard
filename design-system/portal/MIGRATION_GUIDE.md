# Migration Guide: Hardcoded Hex → Design Tokens

This guide maps hardcoded hex color values found across the codebase to their corresponding design tokens. Use it to systematically replace inline hex values with CSS custom properties or Tailwind semantic classes.

**Source of truth**: `src/styles/theme.css`

---

## 1. Hex → Token Mapping Table

### Slate Palette (Most Common)

| Hex Value | Tailwind    | CSS Variable                                                  | Semantic Role                                      |
| --------- | ----------- | ------------------------------------------------------------- | -------------------------------------------------- |
| `#020617` | `slate-950` | `--color-bg-container` (dark)                                 | Outermost dark container                           |
| `#0f172a` | `slate-900` | `--color-panel` (dark)                                        | Dark panels, navbar                                |
| `#101922` | custom      | `--app-chrome-bg` (dark) · `--color-page-body` (dark)         |
| `#1e293b` | `slate-800` | `--color-surface` (dark)                                      | Default dark surface                               |
| `#1a2027` | custom      | `--color-surface` (dark)                                      | Dark surface variant (use `--color-surface`)       |
| `#233648` | custom      | `--color-bg-subtle` (dark) or empty state bg                  | Empty state tinted background                      |
| `#283039` | custom      | `--color-bg-subtle` (dark)                                    | Dark subtle bg (use `--color-bg-subtle`)           |
| `#334155` | `slate-700` | `--color-border` (dark), `--color-bg-subtle` (dark)           | Dark borders, subtle fills                         |
| `#475569` | `slate-600` | —                                                             | Dark mode interactive elements (Ant Design tokens) |
| `#64748b` | `slate-500` | `--color-text-tertiary` (dark), `--color-muted`               | Muted text, placeholder                            |
| `#94a3b8` | `slate-400` | `--color-text-secondary` (dark)                               | Dark secondary text                                |
| `#cbd5e1` | `slate-300` | —                                                             | Light borders, muted elements                      |
| `#e2e8f0` | `slate-200` | `--color-card-border` (light)                                 | Light card borders                                 |
| `#f1f5f9` | `slate-100` | `--color-surface-muted` (light), `--color-heading` (dark)     | Light muted surface                                |
| `#f8fafc` | `slate-50`  | `--color-bg-container` (light), `--color-text-primary` (dark) | Lightest bg, dark mode text                        |
| `#ffffff` | `white`     | `--color-surface` (light)                                     | Light surface                                      |

### Status Colors

| Hex Value                         | CSS Variable               | Use Instead                                                           |
| --------------------------------- | -------------------------- | --------------------------------------------------------------------- |
| `#22c55e` / `#4ade80`             | `--color-status-success`   | `var(--color-status-success)` or `<StatusBadge status="success">`     |
| `#ef4444` / `#f87171`             | `--color-status-failed`    | `var(--color-status-failed)` or `<StatusBadge status="failed">`       |
| `#3b82f6` / `#60a5fa`             | `--color-status-running`   | `var(--color-status-running)` or `<StatusBadge status="running">`     |
| `#f59e0b` / `#fbbf24` / `#eab308` | `--color-status-scheduled` | `var(--color-status-scheduled)` or `<StatusBadge status="scheduled">` |
| `#6b7280`                         | `--color-status-pending`   | `var(--color-status-pending)` or `<StatusBadge status="pending">`     |
| `#f97316`                         | `--color-accent-600`       | `var(--color-accent-600)`                                             |

### Brand / Primary

| Hex Value | CSS Variable                   | Note                                                                       |
| --------- | ------------------------------ | -------------------------------------------------------------------------- |
| `#3b50ce` | `--color-primary-600`          | Solid fill (Button, focus ring, link base). Role A — see COLOR_SYSTEM §1.5 |
| `#e0e7ff` | `--color-primary-container`    | Tinted soft surface for chips/tags/active states. Role B                   |
| `#1e1b4b` | `--color-on-primary-container` | Readable text/icon on primary container. Paired with above                 |

**Legacy tokens removed in v1.1.0** (do not reintroduce):

- `--color-primary-50` — migrated to `--color-primary-container`
- `--color-primary-700` — migrated to `--color-on-primary-container` or `--color-heading` depending on role
- `--color-chip-bg` — replaced by `--color-primary-container`

---

## 2. Anti-Pattern → Token Replacement

### Pattern A: `isDark` Ternary with Hex

**Before**:

```tsx
style={{ color: isDark ? '#94a3b8' : '#64748b' }}
style={{ backgroundColor: isDark ? '#1e293b' : '#ffffff' }}
```

**After** (CSS variable — auto-switches):

```tsx
style={{ color: 'var(--color-text-secondary)' }}
style={{ backgroundColor: 'var(--color-surface)' }}
```

**Best** (Tailwind class — no inline style):

```tsx
className = 'text-[var(--color-text-secondary)]';
className = 'bg-[var(--color-surface)]';
```

### Pattern B: Tailwind Arbitrary Dark Hex

**Before**:

```tsx
className = 'dark:bg-[#233648]';
className = 'dark:bg-[#101922]';
className = 'dark:bg-[#233648]/30';
```

**After** (CSS variable):

```tsx
className = 'bg-[var(--color-bg-subtle)]';
className = 'bg-[var(--app-chrome-bg)]';
className = 'bg-[var(--color-bg-subtle)]/30';
```

**Best** (Tailwind semantic class, requires Phase 1.2 token expansion):

```tsx
className = 'bg-subtle';
className = 'bg-chrome';
```

### Pattern C: Ant Design Component Token Override

**Before** (in `themes/index.tsx` or `ConfigProvider`):

```tsx
Button: {
  defaultBg: 'transparent',
  defaultBorderColor: '#475569',
  defaultHoverBg: '#1e293b',
}
```

**After** (centralized in `src/themes/index.tsx`):
This is the ONE acceptable location for hardcoded hex in Ant Design dark tokens, because Ant Design component tokens do not resolve CSS custom properties. Keep all dark Ant Design overrides consolidated in `src/themes/index.tsx`.

### Pattern D: Status Color Inline Hex

**Before**:

```tsx
const STATE_HEX_COLORS = {
  ok: '#22c55e',
  error: '#ef4444',
  running: '#3b82f6',
};
```

**After** (CSS variable reference):

```tsx
const STATE_COLORS = {
  ok: 'var(--color-status-success)',
  error: 'var(--color-status-failed)',
  running: 'var(--color-status-running)',
};
```

**Best** (use StatusBadge component):

```tsx
<StatusBadge status="success" />
```

### Pattern E2: Tinted primary surface with readable text (MD3 container)

This was the most widespread cause of dark-mode readability regressions in the portal. The fix is always the same: replace the `bg-primary/N + text-primary` / `--color-primary-50 + --color-primary-600` pattern with the container pair.

**Before** (Tailwind, dark mode blue-on-blue):

```tsx
<span className="bg-primary/20 text-primary rounded-full">enriched</span>
<span className="bg-primary/10 text-primary dark:bg-primary/20">selected</span>
<div className="bg-primary/10 text-primary hover:bg-primary/20">...</div>
```

**After**:

```tsx
<span className="bg-primary-container text-on-primary-container rounded-full">enriched</span>
<span className="bg-primary-container text-on-primary-container">selected</span>
<div className="bg-primary-container text-on-primary-container hover:brightness-95">...</div>
```

**Before** (CSS):

```css
.chip {
  background: color-mix(in srgb, var(--color-primary-50) 55%, transparent);
  color: var(--color-primary-600);
}
.dark .chip {
  background: color-mix(in srgb, var(--color-primary-600) 16%, transparent);
  color: var(--color-primary-50); /* BROKEN: resolves to slate-800 in dark */
}
```

**After** (single rule, auto-switches):

```css
.chip {
  background: var(--color-primary-container);
  color: var(--color-on-primary-container);
}
```

### Pattern E3: "Darker on hover" that uses primary shades

```css
/* BROKEN — primary-700 inverts to a LIGHTER indigo in dark mode */
.my-btn:hover {
  background: var(--color-primary-700);
}

/* CORRECT — mixes with black consistently in both themes */
.my-btn:hover {
  background: color-mix(in srgb, var(--color-primary-600) 85%, black);
}
```

### Pattern F: Entity Type Color Hex

**Before**:

```tsx
style={{ color: '#5E7BAE' }}  // dataset blue
style={{ backgroundColor: '#E6ECF3' }}
```

**After**:

```tsx
import { getEntityColors, getEntityTagStyle } from '@constants/entityColors';

const colors = getEntityColors('dataset');
style={{ color: colors[600] }}
// or
style={getEntityTagStyle('dataset', isDark)}
```

---

## 3. Remaining Work

The original priority list (settings, login, sidebar, ontology builder, workflow run history, list empty states) is **fully migrated**, and so is the app-code residual that followed it. What remains is the library-literal floor described below, measured by `npm run lint:colors`.

### Cleared

| File                                                    | Was | Now                                                                                  |
| ------------------------------------------------------- | --- | ------------------------------------------------------------------------------------ |
| `src/pages/agents/agents.css`                           | 8   | `--color-agent-*` definitions moved to `theme.css`; the page file only consumes them |
| `src/pages/collections/.../DatasetDetail/SqlEditor.tsx` | 2   | `bg-surface` — matches `WorkflowCodeDetail`, the sibling Monaco panel (ADR-0182)     |
| `src/styles/components/collections.css`                 | 2   | `var(--color-text-primary)`                                                          |
| `src/styles/components/templates.css`                   | 2   | `var(--color-surface)` / `var(--color-primary-600)` — identical resolved values      |
| `src/styles/components/workflow.css`                    | 1   | `hsl(var(--primary-foreground))` on the primary-filled control                       |
| `src/components/ai/AISelectionMenu.tsx`                 | 1   | `bg-[color-mix(in_srgb,var(--color-surface)_95%,transparent)]` + `border-default`    |

A token-shaped name is not a token: the `--color-agent-*` set was already declared as custom properties, just in a page CSS file holding raw hex. Definitions belong in `theme.css`, which is where light/dark pairs and the lint exclusion both live.

Tailwind's `/N` opacity modifier does **not** work on this repo's semantic color classes — `tailwind.config.js` maps them to whole `var(--color-*)` values, not channel triples, so `bg-surface/95` silently drops the alpha. Use an arbitrary `color-mix()` value when a tokenized surface needs transparency.

### The floor — 15 library literals

These pass a literal color into a library that cannot read a CSS custom property (Leaflet markers, three.js sprites, canvas export backgrounds, chart series fallbacks). They stay in the count deliberately; converting them requires resolving the token to a computed value first.

`components/map/BaseMapView.tsx` · `components/ai/visualizations/ChatCharts.tsx` · `pages/dashboard/components/widgets/MapWidget.tsx` · `pages/dashboard/utils/{downloadPng,thumbnailStore}.ts` · `pages/ontology/graph-explorer/components/GraphVisualization{,3D}.tsx` · `pages/workflows/sections/FlowCanvas.tsx`

### Excluded from the check

`check-hardcoded-colors.mjs` skips files that exist to define a palette: `theme.css`, `index.css`, `themes/index.tsx`, `entityColors.ts`, `graphColors.ts`, `chartColors.ts`, `themeColors.ts`, `graph-explorer/constants.ts`, `avatar/{presets.ts,AvatarEditorPopover.tsx}`, `map/HeatmapLayer.tsx`, and `utils/geoUtils.ts`. Adding a file here is a policy decision — do it only when every hex in the file is a sanctioned palette value, not to silence one line.

The ADR-0065 AI brand-mark components (`AIAssistantIcon`, `AIGenerationIcon`, `AILoadingBorder`, `AISparkleIcon`) are handled differently: they are **scanned like any other file**, and only the five brand-rainbow values (`#3B82F6 · #8B5CF6 · #A855F7 · #EC4899 · #22D3EE`) are discounted. An ordinary surface hex inside a brand mark — an arbitrary `dark:bg-[#hex]`, say — still fails. Registering a new mark means adding it to `aiBrandPaletteFiles`; that grants the palette, not blanket immunity.

---

## 4. Reaching zero

The remaining 15 all pass a color into a library that cannot read a CSS custom property. Clearing them means resolving the token to a value first, not deleting the literal:

```tsx
// Leaflet / three.js / canvas need a concrete color string
const surface = getComputedStyle(document.documentElement)
  .getPropertyValue('--color-surface')
  .trim();
```

That trades a static literal for a runtime read which must also re-run on theme change. Worth doing per surface where the color is demonstrably wrong in one theme — not as a sweep to make the counter read zero.

---

## 5. Validation

```bash
npm run lint:colors
```

The check walks `src/**/*.{ts,tsx,css}`, strips comments before matching (issue references like `#1067` and prose about token values are not colors), skips the palette files listed above, and fails when the count exceeds the threshold pinned in `package.json`. The threshold is the current residual, so **any new hardcoded hex fails CI** — lower it whenever a batch lands instead of leaving headroom. `--self-check` runs the comment-stripping assertions on every invocation.
