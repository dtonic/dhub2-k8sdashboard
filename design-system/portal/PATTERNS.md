# Usage Patterns

Guidelines for consistent code authoring across the D.Hub2 Portal codebase. Each pattern section covers the recommended approach, acceptable alternatives, and anti-patterns to avoid.

---

## 1. Dark Mode

The app uses a `class` strategy: `html.dark` + `[data-theme='dark']`. CSS custom properties in `src/styles/theme.css` automatically switch values between light and dark modes.

### Priority Order

| Priority        | Method                  | When to Use                                                                     |
| --------------- | ----------------------- | ------------------------------------------------------------------------------- |
| 1 (Preferred)   | CSS custom properties   | All new code. Variables resolve automatically per theme.                        |
| 2               | Tailwind `dark:` prefix | Static style overrides where a CSS variable doesn't exist. Use semantic tokens. |
| 3 (Last Resort) | JS `isDark` ternary     | Ant Design component token overrides in `ConfigProvider` only.                  |

### Pattern: CSS Custom Properties (Preferred)

CSS variables from `theme.css` auto-switch based on `[data-theme='dark']`. No conditional logic needed.

```tsx
// Background that auto-switches
<div className="bg-[var(--color-surface)]">

// Text that auto-switches
<p className="text-[var(--color-text-primary)]">

// Border that auto-switches
<div className="border border-[var(--color-border)]">

// Using Tailwind semantic classes (from tailwind.config.js)
<div className="bg-surface text-text-primary border-border-default">
```

### Pattern: Tailwind `dark:` Prefix (Acceptable)

Use when the light/dark values don't map to an existing CSS variable.

```tsx
// GOOD: semantic Tailwind classes
<div className="bg-white dark:bg-slate-800">
<p className="text-gray-900 dark:text-gray-100">

// BAD: arbitrary hex values (defeats the purpose of tokens)
<div className="dark:bg-[#233648]">
<div className="dark:bg-[#101922]">
```

### Pattern: JS `isDark` Ternary (Last Resort)

Only for Ant Design component token overrides where CSS variables cannot be injected.

```tsx
// ACCEPTABLE: Ant Design component tokens (CSS vars don't work here)
<ConfigProvider
  theme={{
    components: {
      Button: {
        defaultBg: isDark ? '#1e293b' : 'transparent',
      },
    },
  }}
>

// BAD: inline styles with isDark (use CSS variables instead)
<div style={{ backgroundColor: isDark ? '#1e293b' : '#ffffff' }}>
// GOOD replacement:
<div style={{ backgroundColor: 'var(--color-surface)' }}>
// BEST replacement:
<div className="bg-surface">
```

### Dark Mode Implementation Checklist

When adding a new component:

1. Use CSS variables for all colors — they switch automatically
2. Test with both `light` and `dark` themes
3. Check contrast ratios (4.5:1 minimum for text)
4. Verify interactive states (hover, focus, active) in both modes

### Dark-mode trap: "darker on hover" using primary shade tokens

Deriving hover states by stepping to a "deeper" primary token does **not** work across themes. The shade scales intentionally invert in dark mode for readability, so a value meant to be "darker" in light ends up **lighter** in dark.

```css
/* BROKEN — reversed in dark mode */
.my-btn {
  background: var(--color-primary-600);
} /* dark = #3b82f6 */
.my-btn:hover {
  background: var(--color-primary-700);
} /* dark = #e0e7ff — LIGHTER, wrong direction */

/* CORRECT — color-mix darkens consistently in both themes */
.my-btn {
  background: var(--color-primary-600);
}
.my-btn:hover {
  background: color-mix(in srgb, var(--color-primary-600) 85%, black);
}
```

Same rule for "lighter on hover" — use `color-mix(..., white)`, not a lower-number shade.

---

## Surface elevation ladder (dark)

Dark mode reads depth as brightness: **lighter is closer**. The tokens already encode the order, and their own comments say so — use them in this order rather than picking a slate value by eye.

| Plane     | Token                                   | Dark      | Use                                                                    |
| --------- | --------------------------------------- | --------- | ---------------------------------------------------------------------- |
| Deepest   | `--color-bg-container`                  | `#020617` | Wells and insets — something recessed _below_ the page                 |
| Canvas    | `--color-panel`                         | `#0f172a` | The working area a form or editor sits in                              |
| App plane | `--app-chrome-bg` · `--color-page-body` | `#101922` | Navbar and sidebar (`bg-chrome`), list body (`bg-page-body`)           |
| Raised    | `--color-surface`                       | `#1e293b` | Cards, tables, page headers, editor chrome (header · tab bar · footer) |

**A `sticky` element belongs on the raised plane.** It overlaps content while scrolling, so it must read as being above it. ADR-0236 §7 briefly put the list page header on the _deepest_ token and it became the darkest surface on screen — darker than the table it headed and darker than the navbar. §8 moved it to `--color-surface`, where the editor chrome already was.

Two checks before choosing a ground:

1. **Does this element ever cover other content?** If yes it is raised, never recessed.
2. **What is directly under and around it?** Measure the neighbours in the running app — a value that looks right in isolation can invert the ladder in place. Token declarations are not enough: `--app-chrome-bg` used to declare `#0f172a` while the navbar computed `#101922`, because Ant Design paints that surface from the `LIGHT_CHROME_BG`/`DARK_CHROME_BG` constants in `src/themes/index.tsx`. The token now carries the real value, and the two must move together.

The app plane carries two tokens because the planes coincide in dark but not in light: the navbar/sidebar are `#f6f7f8` in light while a list body is `#ffffff`. Use `bg-chrome` for app chrome and `bg-page-body` for page content ground; do not substitute one for the other on the strength of their dark values matching.

## 1.5 Tinted surface pattern (container / on-container)

Any surface that uses brand tint as **background** and carries **readable text or icons on that background** must use the MD3 container role pair. This is the single most important rule for avoiding dark-mode contrast regressions.

See [COLOR_SYSTEM.md §1.5 Container roles](./COLOR_SYSTEM.md#15-container-roles-md3-pattern) for the full rationale and token values.

### Quick rule

| You're styling...                                   | Use                                                          |
| --------------------------------------------------- | ------------------------------------------------------------ |
| A chip / tag pill                                   | `bg-primary-container text-on-primary-container`             |
| Active tab or selected row bg (with text inside)    | same                                                         |
| Info box / banner                                   | same                                                         |
| Icon chip on a card (rounded bubble behind an icon) | same                                                         |
| Hover tint on a sidebar item                        | same                                                         |
| Solid CTA button                                    | `bg-primary text-primary-foreground` (Role A, not container) |
| React Flow selected edge, focused input border      | `var(--color-primary-600)` (Role A)                          |

### Anti-pattern to never reintroduce

```tsx
// Produces blue-on-blue in dark mode (WCAG fail)
<span className="bg-primary/20 text-primary">tag</span>
<span className="bg-primary/10 text-primary dark:bg-primary/20">tag</span>
```

```css
/* Same failure in CSS */
.chip {
  background: color-mix(in srgb, var(--color-primary-600) 16%, transparent);
  color: var(--color-primary-600);
}
```

Replace with:

```tsx
<span className="bg-primary-container text-on-primary-container">tag</span>
```

```css
.chip {
  background: var(--color-primary-container);
  color: var(--color-on-primary-container);
}
```

### When the entity is not "primary"

For multi-entity surfaces (dataset / code / pipeline / knowledge detail heroes, entity pills), use the per-entity token `--color-entity-{type}` as an accent. These tokens are already light/dark-inverted, so the existing pattern

```css
--entity-accent: var(--color-entity-dataset);
background: color-mix(in srgb, var(--entity-accent) 18%, transparent);
color: var(--entity-accent);
```

produces readable text on tinted bg in both themes without needing a separate on-container per entity. Do **not** bind `--entity-accent` to `--color-primary-600` or `--color-accent-600` — that reintroduces the blue-on-blue failure.

---

## 1.6 Link text on neutral surfaces

Interactive link-like text placed on **neutral** surfaces (card / panel / app container background) must use the dedicated link tokens, not `--color-primary-600` / `text-primary`.

### Tokens

| Token                   | Light     | Dark      | Contrast on dark card (`#1e293b`) |
| ----------------------- | --------- | --------- | --------------------------------- |
| `--color-link-fg`       | `#3b50ce` | `#a5b4fc` | 7.3:1 (AA)                        |
| `--color-link-fg-hover` | `#192182` | `#c7d2fe` | 9.4:1 (AAA)                       |

Tailwind aliases: `text-link`, `hover:text-link-hover` (and `group-hover:text-link`).

### Quick rule

| Context                                                 | Use                                  |
| ------------------------------------------------------- | ------------------------------------ |
| `<a>` tag on a card / page / panel                      | Inherits from global `a {}` — done   |
| `<button>` styled as a link on a neutral surface        | `text-link hover:text-link-hover`    |
| `shadcn` `<Button variant="link">`                      | Already wired to `text-link`         |
| List card row title on group hover                      | `group-hover:text-link`              |
| Link text ON a primary-tinted container (chip/info box) | Keep `text-on-primary-container`     |
| Link text ON a semantic-tinted banner (info/warn/error) | Keep the level-scoped semantic color |

### Why not `text-primary` / `--color-primary-600`?

`--color-primary-600` resolves to mid-blue `#3b82f6` in dark mode. On neutral slate surfaces (`slate-800`, `slate-900`) that gives ≈ 3.9:1 — **fails WCAG AA for normal text** (4.5:1 required). The `link` tokens ship a dark-mode-safe pair (indigo-300 / indigo-200) that hits AA on every neutral surface while staying in the brand indigo family.

> **Caution — semantic banners.** `<Button variant="link">` and the global `a {}` render with the **neutral-surface** link color. Inside semantic banners (info / warning / error), the link color must match the banner level for semantic alignment. Use a native `<button>` or `<a>` styled with the level-scoped token (e.g. `text-[var(--color-info-600)]`) — do **not** use the neutral `text-link` token there.

### Future enforcement

A follow-up ESLint rule (`no-restricted-syntax`) will flag literal `text-primary` in JSX `className` and suggest `text-link` (link-style text on neutral surface) or `text-text-primary` (body text). Until that lands, new code must follow the quick rule above manually.

✅ **Do**

```tsx
<a href="/docs">Learn more</a>  {/* inherits text-link from global a {} */}
<button className="text-link hover:text-link-hover">View all</button>
<Button variant="link">View collections</Button>  {/* shadcn link variant */}
<p className="font-medium text-text-primary group-hover:text-link">Row title</p>
```

❌ **Don't**

```tsx
<a className="text-primary hover:text-primary/80">Learn more</a>
<button className="text-[var(--color-primary-600)]">View all</button>
<span className="text-[#3b82f6]">Link</span>
<p className="group-hover:text-primary dark:group-hover:text-primary">Row title</p>
```

---

## 1.7 Restrained Selection Surfaces

Selected state should be legible without becoming the loudest object on the page. This matters most for wide surfaces: selected table rows, list rows, cards, provider tiles, capability tabs, and segmented controls. Full-strength primary container fills are acceptable for small chips, but can feel too saturated when repeated across large horizontal rows.

### Quick rule

| You're styling...                                      | Use                                                                                 |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------- |
| Small selected chip / tag / badge                      | `bg-primary-container text-on-primary-container`                                    |
| Checkbox / radio selected indicator                    | `bg-primary text-primary-foreground`                                                |
| Focus ring / active underline / selected edge          | `var(--color-primary-600)`                                                          |
| Wide selected row / card / segmented item              | Low-intensity primary tint + softened border + neutral text                         |
| Wide selected row that repeats many times on one panel | Even lower intensity tint; let the checkbox or count badge carry the strongest cue  |
| Error / warning / success selection                    | Use the semantic state token only when the selected state itself is semantic status |

### Recommended wide-surface class pattern

Use a softened border and partial container tint. Text stays neutral because the background is a quiet state hint, not a full MD3 container surface.

```tsx
const selectedClass =
  'border-[color:color-mix(in_srgb,var(--color-primary-600)_58%,var(--color-border))] ' +
  'bg-[color-mix(in_srgb,var(--color-primary-container)_46%,transparent)] ' +
  'text-text-primary';
```

For dense repeated rows, keep the selected checkbox strong but the row fill quiet:

```tsx
<button
  className={cn(
    'rounded-card border px-3 py-2 text-left',
    isSelected
      ? 'border-[color:color-mix(in_srgb,var(--color-primary-600)_58%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-primary-container)_46%,transparent)]'
      : 'border-border-default hover:border-[var(--input-border-hover)] hover:bg-surface-muted'
  )}
>
  <span
    className={cn(
      'flex size-4 items-center justify-center rounded border',
      isSelected
        ? 'border-[var(--color-primary-600)] bg-primary text-primary-foreground'
        : 'border-[var(--input-border-hover)]'
    )}
  />
  <span className="text-text-primary">Selected item</span>
</button>
```

### Anti-patterns

```tsx
// Too loud when the selected item is a full-width row or repeated card.
<button className="border-primary bg-primary-container text-on-primary-container">
  Selected row
</button>

// Competing semantics: selected state and warning/status color both fight for attention.
<button className="border-[var(--color-warning-600)] bg-[var(--color-warning-50)]">
  Selected capability
</button>
```

Use full `bg-primary-container text-on-primary-container` only when the selected surface is compact or when the selected container itself is the main object, not a repeated row in a dense editor.

---

## 2. Spacing

### Token Scale

| Token         | Value | Tailwind Equivalent |
| ------------- | ----- | ------------------- |
| `--space-2xs` | 2px   | `p-0.5`             |
| `--space-xs`  | 4px   | `p-1`               |
| `--space-sm`  | 8px   | `p-2`               |
| `--space-md`  | 12px  | `p-3`               |
| `--space-lg`  | 16px  | `p-4`               |
| `--space-xl`  | 24px  | `p-6`               |
| `--space-2xl` | 32px  | `p-8`               |
| `--space-3xl` | 48px  | `p-12`              |
| `--space-4xl` | 64px  | `p-16`              |
| `--space-5xl` | 96px  | `p-24`              |

### Guidelines

- Use Tailwind spacing utilities (`p-*`, `m-*`, `gap-*`) for standard spacing — they align with the 4px grid.
- Use CSS variables (`var(--space-md)`) when you need a computed or dynamic value.
- For layout-level dimensions, use layout tokens: `--layout-navbar-height` (50px), `--layout-sidebar-width` (240px), `--layout-sidebar-collapsed` (60px), `--layout-content-max-w` (1400px), `--layout-panel-width` (320px).

```tsx
// GOOD: Tailwind utilities
<div className="p-4 gap-3">

// GOOD: CSS variable for dynamic values
<div style={{ paddingTop: 'var(--layout-navbar-height)' }}>

// BAD: arbitrary pixel values
<div style={{ padding: '16px', gap: '12px' }}>
<div className="p-[16px]">
```

---

## 3. Typography

### Font Families

| Token          | Font      | Tailwind       | Use For                                          |
| -------------- | --------- | -------------- | ------------------------------------------------ |
| `font-sans`    | Inter     | `font-sans`    | Body text, labels, buttons                       |
| `font-display` | Roboto    | `font-display` | Headings (auto-applied via `theme.css`)          |
| `font-mono`    | Fira Code | `font-mono`    | Code blocks, identifiers, technical input values |

Headings are automatically styled with `font-display` via the global CSS rule in `theme.css`. You do not need to manually add `font-display` to heading elements.

### Mono for technical inputs

Apply `font-mono` to input values that a **machine parses** — identifiers, image / registry refs, resource strings (`500m`, `2Gi`), ports / replicas — so they align and read as code. Natural-language fields (name, alias, description) stay `font-sans`. (ADR-0151)

- **shadcn `Input` / `Textarea`** take `font-mono` directly (the class lands on the `<input>`).
- **AntD `InputNumber` caveat** — `className` lands on the _wrapper_, and AntD sets the font on the inner `.ant-input-number-input` through a `:where(…)` rule. Neither `font-mono` nor `[&_input]:font-mono` (specificity 0,1,1) wins, so the number still renders in Inter. Use `[&_input]:!font-mono` (important) to override.

```tsx
// shadcn Input — mono lands directly on the <input>
<Input className="font-mono" placeholder="registry.example.com/team/model:v1@sha256:…" />

// AntD InputNumber — must target the inner input with !important
<InputNumber min={1} className="w-full [&_input]:!font-mono" />
```

Verified via computed style: a plain `font-mono` (or the non-important `[&_input]` variant) on `InputNumber` silently falls back to the sans body font.

**Applied in:** `src/pages/models/edit.tsx` and `src/pages/settings/llm-models/edit.tsx` — both mono the `model`, `image`, `cpu`, `memory` (shadcn `Input`) and `port`, `replicas`, `gpu` (AntD `InputNumber`) fields, and keep `name` / `alias` / `label` / `description` / `health_path` sans.

### Font Size Scale

| Token              | Value | Use For                      |
| ------------------ | ----- | ---------------------------- |
| `--font-size-xs`   | 11px  | Fine print, timestamps       |
| `--font-size-sm`   | 12px  | Captions, small labels       |
| `--font-size-md`   | 13px  | Secondary text               |
| `--font-size-base` | 14px  | Body text (default)          |
| `--font-size-lg`   | 16px  | Emphasized body, subheadings |
| `--font-size-xl`   | 18px  | Section titles               |
| `--font-size-2xl`  | 20px  | Page subtitles               |
| `--font-size-3xl`  | 24px  | Page titles                  |
| `--font-size-4xl`  | 30px  | Hero text                    |

### Line Height

| Token                   | Value | Use For                |
| ----------------------- | ----- | ---------------------- |
| `--line-height-tight`   | 1.2   | Headings, compact text |
| `--line-height-snug`    | 1.35  | Subheadings            |
| `--line-height-normal`  | 1.5   | Body text (default)    |
| `--line-height-relaxed` | 1.7   | Long-form content      |

### Letter Spacing

| Token               | Value   | Use For                  |
| ------------------- | ------- | ------------------------ |
| `--tracking-tight`  | -0.02em | Large headings           |
| `--tracking-snug`   | -0.01em | Medium headings          |
| `--tracking-normal` | 0em     | Body text                |
| `--tracking-wide`   | 0.05em  | Uppercase labels, badges |

### Guidelines

```tsx
// GOOD: use Tailwind typography utilities
<h2 className="text-xl font-bold">Title</h2>
<p className="text-sm text-text-secondary">Description</p>

// GOOD: use CSS variables for custom sizes
<span style={{ fontSize: 'var(--font-size-xs)' }}>Timestamp</span>

// BAD: hardcoded pixel sizes
<h2 style={{ fontSize: '18px', fontWeight: 700 }}>Title</h2>
```

---

## 4. Motion & Animation

### Duration Tokens

| Token                | Value | Use For                              |
| -------------------- | ----- | ------------------------------------ |
| `--duration-instant` | 50ms  | Micro-interactions (opacity toggles) |
| `--duration-fast`    | 150ms | Hover effects, tooltips              |
| `--duration-normal`  | 250ms | Standard transitions                 |
| `--duration-slow`    | 400ms | Complex animations, panel slides     |
| `--duration-enter`   | 300ms | Enter animations (mount)             |
| `--duration-exit`    | 200ms | Exit animations (unmount)            |

### Easing Tokens

| Token           | Value                               | Use For                      |
| --------------- | ----------------------------------- | ---------------------------- |
| `--ease-in`     | `cubic-bezier(0.4, 0, 1, 1)`        | Elements leaving the screen  |
| `--ease-out`    | `cubic-bezier(0, 0, 0.2, 1)`        | Elements entering the screen |
| `--ease-in-out` | `cubic-bezier(0.4, 0, 0.2, 1)`      | Standard transitions         |
| `--ease-spring` | `cubic-bezier(0.34, 1.56, 0.64, 1)` | Bouncy/playful interactions  |

### Guidelines

- Use `tailwindcss-animate` for simple enter/exit animations (already installed as plugin).
- Use `framer-motion` for complex orchestrated animations (page transitions, layout animations).
- Always respect `prefers-reduced-motion`:

```css
@media (prefers-reduced-motion: reduce) {
  *,
  *::before,
  *::after {
    animation-duration: 0.01ms !important;
    transition-duration: 0.01ms !important;
  }
}
```

```tsx
// GOOD: CSS transition with token
<div className="transition-colors" style={{ transitionDuration: 'var(--duration-fast)' }}>

// GOOD: Tailwind animate utility
<div className="animate-in fade-in duration-300">

// GOOD: framer-motion with token-aligned durations
<motion.div
  initial={{ opacity: 0, y: 10 }}
  animate={{ opacity: 1, y: 0 }}
  transition={{ duration: 0.25, ease: [0.4, 0, 0.2, 1] }}
>
```

---

## 5. Empty States

Use a shared `ListEmptyState` component for first-use, widget, and filtered list states.

### Variants

| Variant      | Use For                                     | Rules                                                           |
| ------------ | ------------------------------------------- | --------------------------------------------------------------- |
| `widget`     | Small section blocks (e.g. Home widgets)    | Compact shell, brief copy, one primary CTA                      |
| `initial`    | First-use list pages                        | Title + why-it-matters copy + one primary CTA                   |
| `foundation` | Core activation surfaces (e.g. Collections) | May include helper/secondary action, but still one primary CTA  |
| `filtered`   | Search/filter returned no rows              | No create CTA; only clear/reset/show-all style secondary action |

### Guidelines

- Prefer semantic shells: `bg-empty-state`, `border-border-default`, `text-text-primary`, `text-text-secondary`
- Do not use feature-specific gradient backgrounds inside empty state shells
- Keep one clear primary CTA for first-use states
- Filtered states should restore context, not redirect users into creation flows
- Home/widget empty states should stay compact; avoid large hero copy in narrow containers
- `foundation` variants may include one helper block with 2-3 starter steps for core workspace entry points such as Collections

```tsx
<ListEmptyState
  variant="initial"
  icon={<IconLayoutDashboard size={40} aria-hidden="true" />}
  title={t('dashboard.list.emptyTitle')}
  description={t('dashboard.list.emptyDescription')}
  primaryAction={{
    label: t('dashboard.list.createDashboard'),
    onClick: handleCreateDashboard,
    icon: <IconPlus size={16} />,
  }}
/>
```

---

## 5.3 List Load Error State

Sister surface to §5 Empty States. When a list page's React Query fetch fails, do **not** render an inline red text snippet — surface the failure through the shared `ListLoadError` component so every list page presents the same icon, tone, and copy. See ADR-0057 for the full decision.

### Classifier

`classifyListLoadError(error)` buckets the error into four kinds:

| Kind        | Trigger                                                                                 | Icon                |
| ----------- | --------------------------------------------------------------------------------------- | ------------------- |
| `network`   | `error.message` contains `"failed to fetch"` / `"networkerror"`, or `!navigator.onLine` | `IconCloudOff`      |
| `forbidden` | `ApiError` with `status === 401 \|\| 403`                                               | `IconLock`          |
| `server`    | `ApiError` with `status >= 500`                                                         | `IconAlertTriangle` |
| `generic`   | Everything else (other 4xx, parse failures, unknown)                                    | `IconAlertTriangle` |

ApiClient already handles 401 internally (refresh + sign-out), so the `forbidden` surface is in practice a 403 (insufficient permission) screen.

### Visual rules

Error state uses the **same shell as empty state** — dashed border + `bg-empty-state` — so users see "empty" and "failed" as the same family of states. The icon container stays in the neutral `text-text-tertiary` tone; do not add red emphasis. The retry button uses the `outline` variant.

### i18n

A single shared key set drives all 12 list pages:

```
common.listErrorState.network.{title,description}
common.listErrorState.forbidden.{title,description}    // title interpolates {{resource}}
common.listErrorState.server.{title,description}        // description interpolates {{status}}
common.listErrorState.generic.{title,description}       // title interpolates {{resource}}
```

Each page only contributes `xxx.list.resourceName` (e.g. `dashboard.list.resourceName = "대시보드"`). Do not create page-specific `errorState` key sets.

### Branch order

Always place the error branch **before** loading and empty:

```tsx
const { data = [], isLoading, error, refetch } = useXxxQuery();

return (
  <div className="...">
    {!isLoading && error ? (
      <ListLoadError
        error={error}
        resource={t('xxx.list.resourceName')}
        onRetry={() => refetch()}
      />
    ) : isLoading && filtered.length === 0 ? (
      <TableSkeleton ... />
    ) : (
      <Table ... />
    )}
  </div>
);
```

If `loading` is checked first, the skeleton keeps showing while `error` is truthy and the user never sees the failure.

### Multi-query pages

For pages that aggregate several queries (e.g. Recents):

- Treat partial failures **graciously** — failed domains drop out via `data = []` fallback; surviving domains still render. The user is not blocked.
- Show `ListLoadError` only when **every** query fails. Pass `forceKind="generic"` so the classifier does not try to pick a representative status — a single calm message is the right surface.

### Enrichment queries

When a page has a main query plus row-level enrichment queries (e.g. Knowledge's `useKnowledgeStats` / `useKnowledgeStatusSummaries`), **only the main query** participates in the error branch. Enrichment failures should silently drop the corresponding columns, not block the whole table.

### MSW visual review

Domain mock handlers accept a `?mockError=network|forbidden|server|generic` query via the shared `resolveMockErrorResponse()` helper. In mock mode (`VITE_MOCK_API=true`), changing a single character in the URL reproduces each error case for visual regression checks — no backend cooperation needed.

Avoid handler patterns that overlap with a page route name (e.g. `*/connectors/` collides with the `/settings/connectors` page module). Use `*/api/v1/<domain>/` for new handlers.

---

## 5.4 List Toolbar and Filters

The search/filter strip at the top of a list page must use shared composition:

```tsx
<ListToolbar sticky>
  <ListSearchInput ... />
  <ListSegmentedFilter value={filter} onChange={setFilter} options={options} />
</ListToolbar>
```

Rules:

- Use `ListToolbar` for the list-page strip directly under the page header. It owns the standard horizontal padding, background, sticky offset, and responsive stacking.
- Use `ListSearchInput` for keyword search. Do not use raw `<input>` or Ant Design `Input` for list-page search bars.
- Use `ListSegmentedFilter` for status, type, provider, grant-type, or scope filters with optional counts. Do not hand-roll repeated `<button>` groups with page-local slate/surface classes.
- Use `MultiFilterDropdown` / `SingleFilterDropdown` for option sets that need menus or multi-select behavior.
- Persist list search, segmented filters, dropdown filters, sort controls, and view-mode toggles in URL query state via `useUrlQueryState()` rather than page-local `useState`. Use `q` for keyword search, remove params for default values, and preserve unrelated params such as `tab`, `selected`, and `scope`. This keeps browser back/forward, refresh, and shared links aligned after a row opens a detail route.
- Collection unit filtering is provided by the global navbar collection scope. Collection-scoped resource lists must pass `useCollectionScopeParams()` into their query hook, then apply local search/status/type filtering to that scoped result.
- Do not add a second collection dropdown inside every list toolbar unless the product explicitly asks for a local override. A duplicate control creates ambiguity between global scope and page-local filters.

---

## 5.5 List Name Column (alias-aware 2-tier)

Standardized pattern for rendering the primary Name column in Ant Design `Table` based list pages.

### Principle

- **Line 1**: `alias || name` (font-medium, `text-text-primary`, `group-hover:text-link`)
- **Line 2**: `description` if present, otherwise the internal `name` when an `alias` exists.
  - If neither exists → Line 2 is **omitted** and the row is single-tier.

Line 2 must never duplicate information already rendered in other columns of the same table. Fabricated "meta fallback" strings (e.g. `{mode} · {model}`, `{N} documents`) are **prohibited** because they replay column data and add scan noise.

### Why no synthesized meta fallback?

- Columns already surface the same values and allow sorting/filtering on them
- A repeated "meta" line trains the eye to ignore Line 2, weakening actual `description` scannability
- Row-height consistency is cosmetic; a single-tier row for a pure `name` (no alias, no description) is acceptable

### Why Not Placeholder (`-`, `N/A`, `—`) in Line 2?

- Zero information value; adds visual noise
- Looks like missing data the user must fix
- Column-level null placeholders (`—`) remain acceptable for scalar cell values, but not for the secondary line of a composite Name cell — just omit Line 2 instead.

### Canonical Render

```tsx
render: (_: unknown, record: T) => {
  const hasSecondary = Boolean(record.description || record.alias);
  return (
    <div className="min-w-0">
      <p className="font-medium text-text-primary group-hover:text-link transition-colors truncate">
        {record.alias || record.name}
      </p>
      {hasSecondary && (
        <p className="text-xs text-text-tertiary mt-0.5 truncate">
          {record.description || record.name}
        </p>
      )}
    </div>
  );
},
```

For domains without an `alias` field (e.g. Dashboard), Line 2 is strictly `description` — omit entirely when absent.

### Sorting

Sort by the effective display value, not the raw name:

```ts
sorter: (a, b) => (a.alias || a.name).localeCompare(b.alias || b.name);
```

### Search

Include `alias` in search predicates alongside `name` and `description`:

```ts
item.name.toLowerCase().includes(q) ||
  item.alias?.toLowerCase().includes(q) ||
  item.description?.toLowerCase().includes(q);
```

### Delete / Aria Labels

Use the effective display value for confirmation modals and aria labels:

```ts
content: t('common.modals.deleteModal.confirmMessage', {
  itemName: record.alias || record.name,
});
getAriaLabel: (record) =>
  `${t('common.buttons.actions.moreOptions')} — ${record.alias || record.name}`,
```

### Column Redundancy Rule

Whatever ends up in Line 2 must be **non-column** information:

- `description` is always column-free (no table exposes `description` as a separate column).
- `name` shown as Line 2 when an `alias` exists is the internal identifier — also non-column.
- Do not add synthesized composite strings derived from columns just to keep a 2-tier shape.

### Out of Scope

This pattern applies to **Table-based list pages**. Not applicable to:

- File-browser style lists with an icon column (e.g. Collections Explorer)
- Card grids (e.g. Connectors, Dashboard widget thumbnails)
- User/Group lists that have no `description` field use the same rule: Line 2 is `name` only when alias exists, otherwise omitted.

---

## 5.6 List Table Column Order

Table-based resource lists should keep the governance context close to the resource name so users can scan ownership, collection scope, and operational details without jumping across the table.

### Default Order

Use this order for collection-scoped resources:

```text
Name → Collection → domain-specific columns → Owner/Updated → Actions
```

- The `Name` column remains first and owns the primary row identity.
- The `Collection` column comes second because it is a governance and navigation context, not a secondary metric.
- Domain-specific columns include type, confirmation policy, variable count, endpoint, protocol, key count, or status.
- Owner/date columns stay near the right edge; row actions remain right-aligned and fixed when the table scrolls.

### Header Filters and Sorting

Header affordances are defined by column meaning, not by page. A user who sees the same semantic column in two menus should get the same sort/filter behavior (ADR-0129), and that header state should survive detail navigation and refresh through URL persistence (ADR-0165).

| Column meaning                         | Header affordance         | Shared implementation                                 |
| -------------------------------------- | ------------------------- | ----------------------------------------------------- |
| `이름` / `Name`                        | Sort                      | `createNameColumn`                                    |
| `수정일`, `생성일` / date              | Sort                      | `createDateColumn`                                    |
| `컬렉션` / `Collection`                | Sort + value filter       | `createCollectionColumnControls`                      |
| Type, protocol, status, tags, language | Value filter              | `createValueColumnFilter` / `createArrayColumnFilter` |
| Owner                                  | Value filter (multi)      | `createOwnerColumn` with `records` (ADR-0129 §2.3)    |
| Actions                                | No header control         | `createActionsColumn`                                 |
| Counts, aggregate values, URLs/runtime | No default header control | Add only with explicit domain rationale               |

Rules:

- Prefer `DataTable` for managed resource lists so `useUrlTableState` is applied automatically.
- If a managed list must use raw AntD `Table`, call `useUrlTableState` and pass its `columns` / `onChange` result into the table.
- Give every sortable or filterable column a stable `key`; URL params use `tf_<columnKey>` for filters and `ts=<columnKey>:<order>` for sort.
- Build header value filters from the current table row set with `@components/table` helpers; do not create page-local `filters` / `onFilter` copies for standard facets.
- Header filters are secondary facets. Keep primary search, tabs, and toolbar filters when they are part of the page workflow, but do not let the same column expose different header controls in different menus.
- If a standard affordance is omitted because the value is async-only, composite, permission-dependent, or domain-specific, record that exception in the FSD or a tight code comment.
- Managed list tables are user-resizable (ADR-0246). Pass `resizableStorageKey` to `DataTable`, or call `useResizableColumns` after `useUrlTableState` on a raw `Table` and spread its `columns` / `components` / `tableLayout` / `scrollX`.
- Do not hardcode `scroll={{ x: <constant> }}` or set `tableLayout` on a resizable list table — the hook owns both, and `scroll.x` alone does not produce a fixed layout in rc-table.
- Column widths are a personal view preference: they live in `localStorage` only. Never persist them in the URL, on the server, or in a shared document.

### Exceptions

Move another column before `Collection` only when it is the user's first disambiguation axis for that page:

- Heterogeneous feeds such as Recents may use `Name → Type → Collection`.
- Settings-only provider catalogs may use `Name → Provider/Type → Collection` when provider or placement defines the operational identity.
- File-browser or tree/table hybrids may keep the browsing hierarchy ahead of collection metadata.

Do not put `Collection` after low-level metrics such as variable count, parameter count, key count, or target URL. Those fields refine a resource after the user already understands where it belongs.

---

## 6. Form Patterns

### Unified Input Primitive

**원칙**: 모든 입력은 `@components/ui/` 의 shadcn primitive(`Input`, `Textarea`, `Select`) 를 사용한다. 레이아웃(모달/페이지/인스펙터) 과 무관하게 동일한 시각 — `--input-bg`/`--input-border`/`--input-focus-ring` 토큰 — 을 공유한다.

금지:

- Raw `<input>` / `<textarea>` + 수동 Tailwind 클래스 (`form-input w-full rounded-lg ...`) 조합
- AntD `Input`/`Select` 직접 import (`import { Input } from 'antd'`)
- `variant="modal"` (Input/Textarea/Select 에서 제거됨 — default 그대로 사용)
- 인라인 `style={{ backgroundColor, borderColor, color }}` 로 폼 배경/보더 지정

예외 (AntD 원본 유지):

- `Input.Password` — 눈 아이콘 토글 기능이 필요할 때. className 으로 `inputVariants()` 를 주입해 동일 토큰 적용.
- `InputNumber` — shadcn 에 대응 primitive 가 없으므로 AntD 유지. 인라인 style 금지, AntD 테마 토큰에 의존.

### Standard Form Field

```tsx
import { FormField } from '@components/ui/form-field';
import { Input } from '@components/ui/input';

<FormField label={t('common.name')} required error={errors.name} description={t('common.nameHelp')}>
  <Input
    value={name}
    onChange={(e) => setName(e.target.value)}
    error={!!errors.name}
    placeholder={t('common.enterName')}
  />
</FormField>;
```

### Modal Form Field

모달 안에서도 동일. `FormField` 의 `variant="modal"` 은 라벨/설명 색만 변경하며, Input 자체는 default.

```tsx
<FormField label={t('common.name')} required variant="modal">
  <Input inputSize="md" />
</FormField>
```

### AntD Form.Item 래핑

복잡한 검증/의존/동적 필드는 AntD `Form` 을 사용하되, 자식은 shadcn primitive:

```tsx
import { Form } from 'antd';
import { Input } from '@components/ui/input';
import { Textarea } from '@components/ui/textarea';

<Form form={form} layout="vertical" onFinish={handleSubmit}>
  <Form.Item name="name" rules={[{ required: true }]} label={t('common.name')}>
    <Input placeholder={t('common.enterName')} />
  </Form.Item>
  <Form.Item name="description" label={t('common.description')}>
    <Textarea rows={3} />
  </Form.Item>
</Form>;
```

shadcn `Input` 은 AntD `Input` 을 래핑하며 ref 와 `value`/`onChange` 를 forward 하므로 Form.Item 의 controlled-child 규약과 완전히 호환된다.

Required marker policy: keep AntD Form's default required indicator enabled. Do not set `requiredMark={false}` on standard create/edit forms or form modals; if a surface needs to hide required marks, document the local exception next to that form.

### Focus / Hover / Error

- Focus: 테두리 `--input-focus-border` (#3b50ce) + 링 `0 0 0 2px var(--input-focus-ring)` (light 20%, dark 30%).
- Hover: `input`과 `textarea`의 배경은 `--input-bg`를 그대로 유지하고 테두리만 `primary/70`으로 변경한다. hover 배경색을 추가하거나 `--input-bg`와 다른 표면 토큰으로 교체하지 않는다.
- Composite input: 아이콘이나 버튼을 포함하는 복합 입력은 외곽 컨테이너가 border/focus 시각 상태를 소유하며, 내부 native `input`은 투명 배경을 유지한다.
- Error: `error={true}` → `border-red-500` + 에러 링.

### When to Use Which

| Need                                   | Approach                                                |
| -------------------------------------- | ------------------------------------------------------- |
| 단순 폼 (2–5 필드, 검증 없음)          | shadcn `FormField` + React state                        |
| 복잡 폼 (검증, 의존, 동적 필드)        | AntD `Form` + shadcn primitive 자식                     |
| 모달 내부                              | 위와 동일. `variant="modal"` 없음                       |
| Inspector 패널 (checkbox/radio/switch) | `variant="inspector"` (해당 primitive 에 한해 남아있음) |
| 비밀번호 입력 (토글 필요)              | AntD `Input.Password` + `className={inputVariants()}`   |

---

## 6.5 Page & Modal Headers

The same add/edit action must look identical whether it renders as a **page** or a **modal**. One
visual contract, two entry points. See ADR-0083 (`WorkspaceHeader`) and ADR-0089 (`ModalTitle`
`form` variant).

### Quick rule

| Surface                               | Use                         | Notes                                                           |
| ------------------------------------- | --------------------------- | --------------------------------------------------------------- |
| Page / detail / builder header        | `WorkspaceHeader`           | `FormWorkspace` renders it; never hand-roll a page header       |
| Modal: create / edit / save form      | `ModalTitle variant="form"` | Boxed icon + `text-lg font-semibold`; mirrors `WorkspaceHeader` |
| Modal: confirm / delete / help / info | `ModalTitle` (default)      | Lighter `text-xl font-bold` inline-icon title                   |

### Full-page edit title semantics (ADR-0130)

Full-page resource edit headers identify the object being edited, not the action. Use
`getResourceDisplayName(resource, getRouteResourceIdentifier(routeParam))` for `WorkspaceHeader` /
`FormWorkspace` `title`, with the priority `alias -> name -> decoded route identifier`. If `alias`
is the title and `name` differs, show `getResourceNameSubtitle(resource)` in `subtitle` before any
generic page description fallback. Do not use `OOO 편집`, `OOO 수정`, or `Edit OOO` as the H1 once an
existing resource is in edit mode.

Create pages and create/edit modals may keep action-oriented titles because they either do not have a
persisted resource identity yet or need to signal the task quickly inside a smaller surface.

### The shared visual contract (page header & `form` modal)

- **Icon**: boxed `size-9 rounded-card`, default `bg-primary-container text-on-primary-container`.
  For resource/meta pages the box color comes from the 2-tier tone map (see _Header icon box tone_
  below) via `pageKey`; `iconClassName` remains the escape hatch (e.g. agent-mode tint). Tokens
  only — no hex.
- **Icon glyph reflects the resource**: drive it from `getEntityIcon(entityType)` (dataset →
  database, code → code, pipeline → stackshare, knowledge → book, collection → package). For a
  dialog whose type is dynamic (template wizard: dataset _or_ code), compute the icon from the
  active type so icon + title + type badge agree. Never a generic placeholder glyph.
- **Title**: `text-lg font-semibold text-text-primary`. **Subtitle**: `text-sm text-text-secondary`.
- **Vertical alignment**: the icon box and the title/subtitle block are `items-center` — a
  single-line title is centered against the taller icon box, not pinned to its top edge.

### Anti-patterns

- ❌ Hand-rolling a page header (custom back button + inline icon + `text-xl font-bold`) instead of
  `WorkspaceHeader`.
- ❌ Full-page edit H1/title copy such as `LLM 모델 편집`, `시크릿 수정`, or `Edit Prompt` when the
  edited resource has `alias`, `name`, or a route identifier.
- ❌ A generic/placeholder icon that doesn't match the resource being created (e.g. one template
  glyph for both "데이터셋 만들기" and "코드 만들기").
- ❌ `ModalTitle variant="form"` for a confirm/delete dialog — keep those on the default variant.
- ❌ Re-introducing inline `style` color or `var(--modal-text-*)` on the `form` variant title — it
  uses `text-text-primary` to match the page header.

### Header icon box tone — 2-tier (ADR-0158)

The header icon _box color_ is centrally mapped, never hand-rolled. Use **`PageHeaderIcon`**
(`@components/PageHeaderIcon`) on list-page headers, and pass the matching **`pageKey`** to
`WorkspaceHeader` / `FormWorkspace` on resource editors. The old `HEADER_GRADIENT` registry (per-page
saturated gradient + white icon + shadow) is **removed** — it collided across pages, hardcoded raw
palette, and bypassed the entity color tokens. `getPageHeaderIconTreatment(pageKey, isDark)` resolves
the box via two tiers:

| Tier                           | Keys                                                                                                                                      | Box treatment                                                                                                                                               |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **1 — resource types**         | `collection` `dataset` `code` `pipeline` `knowledge` `dashboard` `entity` `relation` `agent` `tool` `actor`                               | Flat tint from `getResourceColor()` (`iconBg` + `iconColor`) — matches the resource's badge/tree/row color and is identical on the list page and its editor |
| **2 — meta / admin / utility** | `settings` `users` `groups` `secrets` `markings` `oidcClients` `connectors` `recents` `labs` `examples` `home` `prompts` + semantic layer | Single neutral `bg-subtle text-text-secondary` (same neutral as SectionCard) — identity comes from the glyph + title, not a decorative hue                  |

```tsx
// List page header — box color from the 2-tier map, glyph stays explicit
<PageHeaderIcon pageKey="knowledge" className="mt-0.5">
  <IconBook2 size={20} aria-hidden="true" />
</PageHeaderIcon>

// Resource editor — WorkspaceHeader / FormWorkspace matches its list page
<WorkspaceHeader title={name} icon={<IconBook2 />} pageKey="knowledge" … />
```

- **Precedence** on `WorkspaceHeader`: `iconClassName` (escape hatch, e.g. agent-mode tint) > `pageKey`
  tone > `bg-primary-container` default.
- **Import via the subpath** `@components/PageHeaderIcon`, not the `@components` barrel — the barrel
  pulls unrelated modules (e.g. `ActionCard`'s top-level `Typography`) into partial-mock page tests
  and breaks them.
- Tier 2 is a deliberate design choice: meta pages are not entities, so a unique hue is decorative
  noise — the neutral box keeps signal on the resource tiers (quiet defaults, structure before color).

### Anti-patterns (header icon box)

- ❌ Hand-rolling the box: `size-9 rounded-lg bg-gradient-to-br … text-white shadow-sm`.
- ❌ Re-introducing `HEADER_GRADIENT` or any per-page gradient registry.
- ❌ Importing `PageHeaderIcon` from the `@components` barrel instead of the subpath.
- ❌ Giving a meta/admin page its own hue instead of the shared neutral.

---

## 6.6 Modal Footer Actions

The footer (action button row) is the **pair of the header** — it follows the same "one contract,
both surfaces" rule. Write the button row with the shared **`ModalActions`** component, never an
inline `<div className="flex justify-end gap-2">`, raw `<button>`, or a copy-pasted
`h-10 min-w-[96px] …` button class. See ADR-0095.

### Quick rule

| Surface                       | Use                           | Chrome (bg/border/padding)            |
| ----------------------------- | ----------------------------- | ------------------------------------- |
| AntD `<Modal>`                | `footer={<ModalActions … />}` | `getStandardModalStyles().footer`     |
| Custom portal modal           | `<ModalActions surface … />`  | `ModalActions` renders it (`surface`) |
| `FormWorkspace.footer` (page) | `<ModalActions … />`          | `FormWorkspace` renders it            |

### The contract (encapsulated in `ModalActions`)

- **Order**: `[leading (mr-auto)] … [cancel] [confirm]`. Cancel always sits left of confirm; the
  action group is right-aligned (`ml-auto`).
- **Cancel variant**: `ghost` (low-emphasis — design principle 5, "quiet defaults, loud signal").
  Never `outline`.
- **Confirm variant**: `default`; `confirm={{ variant: 'destructive' }}` for delete/irreversible.
- **Size**: standardized `h-10 min-w-[96px] px-5 py-2.5 text-sm font-semibold` — baked in, never
  passed by callers.
- **Loading/disabled**: `confirm.loading` shows a spinner and auto-disables both buttons.
- **Leading slot**: left auxiliary content (progress bar, target path, a secondary destructive
  action like "remove schedule") goes in `leading={…}`.
- **Labels (i18n)**: pass `t('common.buttons.actions.*')` keys. `cancel.label` omitted defaults to
  `common.buttons.actions.cancel`. Don't introduce legacy `common.cancel`/`common.add` keys.

```tsx
// AntD modal — chrome from getStandardModalStyles(), so no `surface`
<Modal footer={
  <ModalActions
    cancel={{ onClick: onClose }}
    confirm={{ label: t('common.buttons.actions.save'), onClick: handleSave, disabled: !canSave }}
  />
} styles={getStandardModalStyles()} />

// Custom portal modal — render the footer chrome itself
<ModalActions
  surface
  className="rounded-b-xl"
  cancel={{ onClick: onClose, disabled: isLoading }}
  confirm={{ label: t('common.buttons.actions.delete'), onClick: onConfirm, variant: 'destructive', disabled: !isConfirmed }}
/>
```

### Anti-patterns

- ❌ Inline `footer={<div className="flex justify-end gap-2">…</div>}` hand-rolled button rows.
- ❌ Raw `<button>` in a modal footer (no loading/icon/focus-ring) — use `ModalActions`.
- ❌ Copy-pasting the `h-10 min-w-[96px] px-5 py-2.5 text-sm font-semibold` button class.
- ❌ `outline` cancel buttons, or a destructive action styled with hardcoded `bg-red-*` instead of
  `confirm.variant='destructive'`.
- ❌ A secondary destructive action floated left with `justify-between` — put it in `leading`.

---

## 6.7 Contextual Action Copy

Action copy follows the context rule from ADR-0117: when the surrounding title already names the
resource, the visible button label should carry the action verb only. Keep the accessible name
specific.

### Quick rule

| Surface                          | Visible text | Accessible name                                        |
| -------------------------------- | ------------ | ------------------------------------------------------ |
| List header: `Collections`       | `Create`     | `Create Collection`                                    |
| List header: `OIDC Clients`      | `Register`   | `Register OIDC Client`                                 |
| List header: `Report Generator`  | `Generate`   | `Generate Report`                                      |
| Modal title: `Create Collection` | `Create`     | Visible label is enough unless extra context is needed |

### Keep the noun when context is weak

- Empty-state CTAs keep the resource noun: `Create Prompt`, `Generate Report`.
- Menus and dropdown options keep the resource noun because the user is choosing among targets.
- Confirmation/destructive copy keeps the target explicit.
- Basic identity field labels use `Name` / `이름`; keep resource-specific constraints in helper
  text, placeholder, or validation messages.

---

## 5.7 Identity hover preview

Per-row resource owner columns in every list page (dashboards, collections, pipelines, ontologies, knowledge, agents) must use the shared owner display so avatar resolution, hover behavior, multiple-owner handling, and legacy-fallback treatment stay identical across the product.

### Canonical render

```tsx
import { OwnerCell, useResourceAccessMetadata } from '@components/permissions';

const { metadataById, isLoading: ownersLoading } =
  useResourceAccessMetadata(items, 'dashboard'); // any resource_type

// In the Ant Design columns array:
{
  title: t('common.labels.owner'),
  key: 'owner',
  width: 150,
  render: (_, record) => (
    <OwnerCell
      owners={metadataById[record.id]?.owners ?? []}
      fallbackName={record.owner?.trim() || undefined}
    />
  ),
}
```

### Rules

- **Always pair** `OwnerCell` with `useResourceAccessMetadata(items, resourceType)`. The hook batches permission lookups in a single `useQueries` call and exposes `metadataById[id].isSharedByPermission` for `my` / `shared` filters and counts.
- **OwnerPreview** is the hover popover — `OwnerCell` wraps its trigger in it automatically. Use `OwnerPreview` directly only when the trigger element is custom (e.g. a detail-header owner chip).
- **Never hand-roll** a second popover, tooltip, or avatar-plus-name row for owners. If the preview contents need to change, extend `OwnerPreview` rather than forking it per page.
- **`fallbackName`** is for legacy records where the backend only stored a `owner` string — treat it as a synthetic user subject. Remove the fallback once the backend guarantees permission grants for every resource.
- **N+1 caveat**: the hook fires one permission query per row. Acceptable for 20–50 rows; a bulk endpoint should replace it if list sizes grow.

### Anti-pattern

```tsx
// BAD — bespoke owner cell per feature
<div className="flex items-center gap-2">
  <div className="size-6 rounded-full bg-primary text-white ...">{name.charAt(0)}</div>
  <span>{name}</span>
</div>
```

---

## 6. Inline Style Rules

### Prohibited

Inline `style={{}}` for colors, backgrounds, and borders:

```tsx
// BAD
<div style={{ color: '#6b7280', backgroundColor: isDark ? '#1e293b' : '#fff' }}>
<span style={{ borderColor: '#e2e8f0' }}>
```

### Allowed Exceptions

Inline styles are acceptable ONLY for:

1. **Dynamic computed values** that cannot be expressed in Tailwind:

   ```tsx
   style={{ transform: `translateX(${offset}px)` }}
   style={{ width: `${percentage}%` }}
   style={{ gridTemplateColumns: `repeat(${cols}, 1fr)` }}
   ```

2. **Drag/resize positions** from pointer events:

   ```tsx
   style={{ left: dragX, top: dragY }}
   ```

3. **Canvas/chart library requirements** (React Flow node positions, ECharts dimensions):

   ```tsx
   style={{ position: 'absolute', ...nodePosition }}
   ```

4. **Ant Design ConfigProvider component tokens** where CSS variables cannot be used:
   ```tsx
   // Acceptable — Ant Design component tokens don't resolve CSS vars
   components: {
     Button: {
       defaultBg: isDark ? '#1e293b' : 'transparent';
     }
   }
   ```

### Refactoring Inline Styles

When you encounter existing inline styles:

```tsx
// Before
<div style={{ padding: '16px', marginBottom: '24px', borderRadius: '8px' }}>

// After
<div className="p-4 mb-6 rounded-lg">
```

```tsx
// Before
<p style={{ color: isDark ? '#94a3b8' : '#6b7280', fontSize: '12px' }}>

// After
<p className="text-[var(--color-text-secondary)] text-sm">
```

---

## 11. Destructive Action Confirm

되돌릴 수 없는 액션(삭제 등)을 실행하기 전에 사용자 확인이 필요하면 **`Modal.confirm`을 직접 쓰지 말고 `confirmDestructive()` 유틸을 사용한다.**

`Modal.confirm`은 AntD imperative API로 React 렌더 트리 밖에서 동작해 `getStandardModalStyles()` 등 디자인 시스템 스타일이 적용되지 않는다. `confirmDestructive()`는 `ConfirmDestructiveModal`(앱 루트에 마운트된 상태 기반 컴포넌트)을 통해 디자인 시스템 토큰과 다크/라이트 모드를 올바르게 적용한다.

```tsx
import { confirmDestructive } from '@utils/confirmDestructive';

confirmDestructive({
  title: t('actions.deleteConfirm.entityTitle'),
  content: t('actions.deleteConfirm.entityMessage', { name: displayName }),
  okText: t('actions.deleteConfirm.okText'),
  cancelText: t('actions.deleteConfirm.cancelText'),
  onOk: async () => {
    await someApi.delete(id);
  },
});
```

기존 i18n 키 `actions.deleteConfirm.*`에 entity/relation/bulk 삭제 문구가 준비돼 있으니 재사용한다.

> **Anti-pattern**: `Modal.confirm({ ... })` 직접 호출 — 디자인 시스템 스타일 미적용.

---

## 7. Contribution Checklist

When adding or modifying design system artifacts:

### Adding a New Token

1. Add the CSS variable to `src/styles/theme.css` (both `:root` and `[data-theme='dark']`)
2. Update `docs/design-system/tokens.json` with the W3C DTCG format entry
3. Update `docs/design-system/COLOR_SYSTEM.md` with the new token in the appropriate table
4. If the token should be available as a Tailwind class, add it to `tailwind.config.js`
5. Record it under `Unreleased` in `docs/design-system/CHANGELOG.md` — the resolved light/dark values and why. That file tracks the token layer specifically, and steps 2–4 are exactly where a value silently drifts out of sync.

Define tokens in `theme.css`, never in a page or component stylesheet. A custom property declared next to the rules that use it still looks like a token and still needs a light/dark pair, but it sits outside the file the rest of the system — and `lint:colors` — treats as the source of truth.

### Adding a New Component

1. Create the component in `src/components/ui/` (for primitives) or `src/components/` (for shared)
2. Use CVA for variant management, `cn()` for class merging
3. Support `modal` variant if the component will be used in modals
4. Add the component to `docs/design-system/COMPONENTS.md`
5. Export from the barrel file (`src/components/ui/index.ts`)

### PR Checklist

- [ ] No hardcoded hex colors — run `npm run lint:colors`
- [ ] Token added, removed, or re-valued? Recorded in `CHANGELOG.md` under `Unreleased`
- [ ] Dark mode tested (both themes)
- [ ] Uses CSS variables or Tailwind semantic classes
- [ ] No unnecessary inline `style={{}}` attributes
- [ ] i18n: all user-facing text uses `t()`
- [ ] Component reuse: checked existing components before creating new ones
- [ ] Accessibility: `aria-label` on icon-only buttons, `aria-live` on dynamic content

---

## 10. Loading States (Skeleton over Spin)

### Principle

Cold-load states reveal layout **structure**, not activity. Skeleton placeholders preserve the page's visual rhythm while data arrives; a centered `Spin` collapses the layout into a single spinner and erases every signal the user came to the page for.

### Quick rule

- **Page / section / list-table cold load** → Skeleton (shape-matched to the real content).
- **Inline affordance** (submit button while saving, inline status chip) → small `Spin` or `IconLoader2`.
- **Background refetch on already-populated content** → no overlay. The stale content stays visible; do not dim it with a Spin.
- **AI-generation content placeholder** (waiting on model output that will fill a content area) → `AISkeleton` (brand-shimmer). It is the only skeleton that carries the AI brand rainbow, so an "AI is generating" wait reads differently from a plain data load. Do **not** use it for data loads, and do **not** replace the other AI-wait affordances with it (see below).

### List tables — use `TableSkeleton`

```tsx
import { TableSkeleton } from '@components';

{isLoading && items.length === 0 ? (
  <TableSkeleton rows={8} columns={5} />
) : (
  <Table dataSource={items} columns={columns} ... />
)}
```

- Do **not** pass `loading` to Ant Design `Table` for cold loads — its internal Spin destroys the column rhythm.
- Choose `columns` to match the visible column count (ignore the action dropdown column).
- Guard on `items.length === 0` so a background refetch doesn't replace rendered rows with a skeleton.

### Full-page / panel cold load

Use `Skeleton` from `antd` with `paragraph.rows` and `title.width` shaped to the real content. For stacked item lists inside a panel, use several `Skeleton.Input` blocks with explicit heights (see `ToolSelector`, `ActorSelector`, `RoleSelector` in `src/pages/agents/builder/react/components/`).

```tsx
if (isLoading) {
  return (
    <div className="flex-1 flex flex-col h-full overflow-hidden">
      <div className="px-8 py-6 border-b border-border-default">
        <Skeleton active paragraph={{ rows: 1, width: ['40%'] }} title={{ width: '25%' }} />
      </div>
      <div className="flex-1 p-8">
        <Skeleton active paragraph={{ rows: 6 }} />
      </div>
    </div>
  );
}
```

### Tree / custom layouts

Shape a dedicated skeleton that mirrors the layout. The collections left tree ships `CollectionTreeSkeleton` (indentation + chevron + label rows) — reuse it. Do **not** stack a Spin over a partially-rendered tree during a background refresh.

### AI generation — use `AISkeleton`, but only for content placeholders

When the app is waiting on **model output that will fill a content area** (e.g. the TTL condense draft, an AI-drafted body preview), use `AISkeleton` from `@components/ai` instead of the quiet AntD `Skeleton`. It renders shimmer lines carrying the brand rainbow (`#3B82F6 → #A855F7 → #EC4899 → #22D3EE` — the same palette as `AISparkleIcon` / `.ai-sweep-border`, a CLAUDE.md DON'T #1 brand-fixed exception), so the "AI is generating" moment is visually distinct from a plain data load. Motion is disabled under `prefers-reduced-motion`.

```tsx
import { AISkeleton } from '@components/ai';

{
  isGenerating ? <AISkeleton lines={6} /> : <ResultPreview data={result} />;
}
```

**Decision rule — pick the AI-wait affordance by surface, do not force `AISkeleton` everywhere:**

| AI wait surface                                         | Affordance (already branded)            |
| ------------------------------------------------------- | --------------------------------------- |
| Inline field generation (alias/description/tags/…)      | `.ai-sweep-border` via `InlineAIField`  |
| Boxed prompt/code generation                            | `AILoadingBorder`                       |
| Chat response generation                                | `ThinkingIndicator`                     |
| Long backend generation job (report)                    | `GenerationProgress` (progress)         |
| **Skeleton-shaped placeholder for incoming AI content** | **`AISkeleton`**                        |
| Plain data load (chat history, list, panel), no model   | quiet AntD `Skeleton` / `TableSkeleton` |

Replacing a purpose-built affordance (sweep border, loading border, thinking indicator) with `AISkeleton` is a downgrade — only reach for `AISkeleton` where you would otherwise show a neutral skeleton for content the model is about to produce.

### Anti-patterns

- ❌ `<Table loading={isLoading} />` for cold loads — shows Spin over an empty body.
- ❌ Full-page `<Spin size="large" />` as a route fallback — use a Skeleton shaped to the route's chrome instead.
- ❌ Spin overlay on already-rendered content during background refetch — leave the content visible.
- ❌ A single generic Skeleton block unrelated to the real layout — match the shape of what's coming.

### Coverage (2026-04-24)

Migrated: Recents, Dashboard list, Collections tree refresh overlay, Pipelines list, Ontology (Modeling) list, Graph Explorer metadata panel, Knowledge list, Agents list + sub-pages (tools, actors, builder, chat, ToolSelector/ActorSelector/RoleSelector, ToolsActorsTab, NodeInspector fallback), Reports list, Settings Users & Groups.

---

## 12. Tab Icon Convention (ADR-0104)

Tab labels are **text-only** across the app. This covers both tab implementations — the shared `EditorTabs` (`src/components/EditorTabs.tsx`, Type B editors) and Ant Design `Tabs` (detail pages, modals/dialogs, inspectors, sub-panels).

### Rule

- **Navigation / editing / section / detail / modal / inspector tabs → text-only.** No `IconXxx` / `<Icon…/>` in `tabs[].label` or `items[].label`. This explicitly includes the `권한 / Permissions` tab — use `t('common.permissions')`, never an `IconLock` + text label.
- **One strip, one treatment.** A single tab strip must never have icons on some tabs and text on others (Material Design tabs consistency rule). The lone-icon permission tab was the main pre-ADR-0104 defect.
- **Permission tab i18n is unified** on `t('common.permissions')` (= "공유 및 권한" / "Share & Permissions").
- **Count badges are allowed** (e.g. `<AppTag>{n}</AppTag>` after the text) — a badge is not an icon.

### Exception: view-mode switchers

A strip that toggles **representations of the same content** (e.g. the Graph Explorer `GraphExplorerPage` 2D/3D graph toggle and table / text result panel) is semantically a **segmented control**, not navigation, and **may use icons**. When adding a new strip, decide by intent: "move to different content" → text-only tab; "change how the same content is shown" → icon-allowed view switcher.

### Anti-patterns

- ❌ `IconLock` (or any icon) on only the permissions tab while sibling tabs are text — mixed strip.
- ❌ Iconifying every tab of a detail page (e.g. the old `KnowledgeDetailPage` File/Message/Search/Settings/Lock strip) when peer detail pages are text-only.
- ❌ Per-feature `*.tabs.permissions` i18n keys for the permission tab — use `common.permissions`.

### Reference

ADR-0104 (탭 아이콘 컨벤션), supersedes ADR-0102 §1's `IconLock` permission-tab instruction. See `docs/design-system/COMPONENTS.md` Type B rules.

## 12.5 Input Parameter Editor

Use `InputParameterEditor` for ordered definitions that describe values entering an executable
resource or workflow node: Tool/Actor invocation parameters, Start-node variables, and Human Input
fields. This is distinct from the data-table `SchemaEditor` in §13 and from runtime argument forms
that collect concrete values from a user.

### Layout and interaction

- Wide containers use one flat bordered grid: **Name → Type → Description → Required → Actions**.
- Narrow inspector containers automatically switch to stacked fields with persistent labels; do not
  hide labels or force a horizontally scrolling desktop table into the inspector.
- Parameter names use mono typography because they are machine-consumed identifiers.
- Descriptions auto-grow up to three lines; do not truncate schema guidance into an unreadable
  single-line input.
- Add is a compact outlined action above the list. Delete remains visibly available in every row and
  must never depend on mouse hover.
- Every Input, Select, Switch, and delete button needs a row-specific accessible name.
- Empty and duplicate names render inline errors. Full-page save actions call
  `hasInputParameterIssues()` before serializing the schema.

### Variants

- `density="default"`: full-page Tool/Actor sections.
- `density="compact"`: workflow inspector panels.
- `showDescription={false}`: Human Input fields, whose current contract has no edited description.
- Omit `onChange` and pass `readOnly` for built-in resources.
- Prompt template declarations keep using the already-shared `PromptVariablesEditor`; their
  **Name → Default → Description → Required** contract has no type selector and is intentionally
  distinct from executable input definitions.

### Anti-patterns

- ❌ Per-feature copies of the same name/type/required row editor.
- ❌ Placeholder-only fields; entered values erase their meaning.
- ❌ Rounded card per row inside an already bordered `SectionCard`.
- ❌ Hidden hover-only destructive actions.
- ❌ Using this component for Arrow/ontology dataset schemas or for concrete runtime argument values.

## 13. Schema / Attributes Editor (ADR-0139)

One row-based editor for every data-table schema surface — ontology entity/relation modals & inspectors, the workflow attributes tab, the dataset-create wizard, and the dataset detail Schema tab. Invocation/input definitions use `InputParameterEditor` (§12.5). Components live in `src/components/schema/`: `SchemaEditor` (control bar + mode switch) wraps `SchemaUIMode` (the row list); datasets use `DatasetSchemaEditor`, a thin wrapper over `SchemaUIMode`. Feature pages must import this shared module instead of another page's internal implementation.

### Row anatomy

A CSS grid (`role="table"`/`row"`/`gridcell"`, not `<table>`) with a shared column template so header and rows align: **Name → Alias → Description → Data type → Nullable → Actions**. Per row:

- **Type-category icon** (`SchemaTypeIcon`) — tinted by data-type family (text/number/temporal/boolean/complex/binary) via `getSchemaTypeColor`. Tokens: `--color-schema-type-*` in `theme.css` (light + dark). Never hardcode.
- **`TypePicker`** — grouped, searchable type combobox (AntD `Popover`) with type-family icons and native-name hints (Airtable/Notion style), replacing the flat AntD Select. Honors `showNativeTypes`.
- **Inline constraint badges** — `NOT NULL` / `NULL` badge **toggle** (not a checkbox); inline **PK** amber `🔑 n` badge toggle for ontology (composite key order = drag the key chips in the slim strip). Datasets don't render PK/identity — the only cross-surface difference.
- **Editable JSONPath** (REST) — `jsonPathEditable` turns the `{}` marker into a click-to-edit popover; otherwise it's a read-only marker.
- **Drag-to-reorder** — grip handle (native HTML5 DnD) since field order is the Arrow serialization order; keyboard fallback via `ArrowUp`/`ArrowDown` on the focused grip. Reordering is clamped to the editable region (reserved `id` / read-only / persisted rows stay put).
- **Locking** — `lockedRowIds` locks name/type/nullable/PK/delete on persisted rows; alias/description stay editable.

### Rules

- **Data contract is invariant.** `SchemaRow`/`SchemaMetadata`, the Arrow save payload (`buildArrowType` + `metadata.{alias,comment,jsonpath}`, `metadata.keys` comma order + `display_column`), and validators are unchanged across the redesign. For datasets, the Arrow↔SchemaRow adapter (`schemaRowAdapter`) preserves each **untouched** column's exact native Arrow type on save (no int8→int16 widening).
- **Large schemas** (> 40 rows) get `content-visibility:auto` + `contain-intrinsic-height` on each row (native rendering virtualization — no windowing library, so drag/keyboard/focus keep working). Below the threshold it's a no-op.
- Mode label is **`Table`** (not `UI`); single-purpose surfaces pass `availableModes` to hide unused JSON/CSV modes.

### Anti-patterns

- ❌ A second bespoke schema editor (the old `EditableSchemaTab`/`useEditableSchema` — deleted). Reuse `SchemaEditor`/`SchemaUIMode`.
- ❌ Hardcoded type colors — use `--color-schema-type-*` / `getSchemaTypeColor`.
- ❌ Nullable as a checkbox or PK as a separate top multiselect panel — use the inline badge toggles.

### Reference

ADR-0139 (모던 재설계·데이터셋 통일), supersedes ADR-0122's UI model. See `COMPONENTS.md` for the component catalog entry.
