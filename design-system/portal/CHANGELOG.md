# Design System Changelog

**Scope: the token layer.** From 1.3.0 onward this file records changes to design tokens — `src/styles/theme.css`, `docs/design-system/tokens.json`, and the semantic color/spacing/radius mappings in `tailwind.config.js`. Added, removed, renamed, or re-valued tokens belong here, with the resolved value and the reason.

Everything else about the design system is recorded elsewhere and should not be duplicated here:

| Change                                         | Where it is recorded                         |
| ---------------------------------------------- | -------------------------------------------- |
| Component API, variants, new shared components | `COMPONENTS.md` + the ADR                    |
| Usage patterns, layout, copy rules             | `PATTERNS.md` + the ADR                      |
| A design decision and its rationale            | `docs/ADR/` (indexed in `docs/DECISIONS.md`) |
| Agent-facing rule changes                      | the `AGENTS.md` changelog                    |

The earlier scope — "all notable changes to the design system" — duplicated those four and was kept up in 5 of the 50 commits that touched `docs/design-system/` between 1.2.0 and 1.3.0. A token value that silently drifts out of `tokens.json` is the one failure the other four ledgers do not catch, so that is what this file is for now.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added — tokens

- **`--color-page-body`** — `#ffffff` light, `#101922` dark. Names the plane a list page's content sits on. It existed only as `background-dark` in `tailwind.config.js`, a raw hex outside `theme.css` that 33 files consumed as `bg-white dark:bg-background-dark`; being outside the theme file it could not follow `[data-theme]`. Now mapped as `bg-page-body` and the raw hex is gone.

- **`--color-status-listening` / `-bg`** — `#6d28d9` / `#f5f3ff` (light), `#a78bfa` / `#2e1065` (dark). The violet the pipeline list already used for the `이벤트 대기` pill, promoted out of raw Tailwind palette classes (`!bg-violet-50 … dark:!bg-violet-950/40`). The dark background is now solid like every other `status-*-bg`, so the tag reads slightly denser than the previous 40% alpha.
- **`--color-status-waiting` / `-bg`** — `#0e7490` / `#ecfeff` (light), `#22d3ee` / `#083344` (dark). Same promotion for the cyan `예약됨` / `준비 중` pill. Named `waiting` rather than reusing `--color-status-scheduled`: that token is amber and is already consumed by `VersionsTab`, `ConfigurationPanel`, and `stateColors.ts`, so re-valuing it to cyan would have moved four unrelated surfaces.
- **`--color-status-degraded` / `-bg`** — `#b45309` / `#fffbeb` (light), `#fbbf24` / `#422006` (dark). New state for an event pipeline whose backlog is over the threshold (ADR-0229 in dhub2-manager). It starts on the same amber as `--color-status-scheduled` but stays a separate token because the pipeline list shows both at once — a delayed event pipeline beside a scheduled batch one — so either must be able to move without the other.

### Changed — tokens

- **`--app-chrome-bg`** — corrected to the values the app actually paints: `#f7f8fa` → `#f6f7f8` (light), `#0f172a` → `#101922` (dark). The navbar and sidebar are painted from `LIGHT_CHROME_BG`/`DARK_CHROME_BG` in `src/themes/index.tsx` through Ant Design inline styles, so the token had been describing a colour nothing rendered — measured in the running app, where the navbar computed `#101922` against a token declaring `#0f172a`. Affects the two consumers (`bg-chrome` on the collection tree search field, the pipeline save dropdown in `workflow.css`), which now match the chrome they sit on. The token file carries a note binding it to the TypeScript constants.

- **`--color-list-header-bg` (dark only)** — re-valued from `--color-bg-container` (`#020617`) to `--color-surface` (`#1e293b`); light stays `#ffffff`. The list page header is `sticky`, so it is the element that floats **over** the content — but it was painted with the token whose own comment reads "deepest layer", making it darker than the table it heads (`#1e293b`), darker than the list body (`#101922`), and darker than the navbar and sidebar (`#101922`). Measured in the running app, not inferred. It now sits on the same plane as the content cards below it and as the editor chrome (`WorkspaceHeader`/`FormWorkspace`, also `--color-surface`), so one elevation ladder covers list and editor. `.dark .collections-header-gradient` moved with it — the collection overview and entity detail headers fill the same role. Revises ADR-0236 §7, which chose `#020617` from an A/B that never included `#1e293b`.

- **`--color-agent-header-bg` → `--color-list-header-bg`** — renamed, values unchanged (`#ffffff` light, `#020617` dark). ADR-0236 §7 makes this the ground for **every** list page header, not just the AI family, so the AI-specific name no longer described it. The consuming class moved with it (`.agent-header-bg` → `.list-header-bg`) out of the page-scoped `src/pages/agents/agents.css` into the globally imported `src/styles/components/list-page-header.css` — the connectors list used the class without importing that stylesheet and rendered a transparent sticky header. Migrated list pages move from `#101922` to `#020617` in dark: the body ground is `#101922`, so the header now reads as its own band instead of leaning on the 1px border alone.

- **`--color-status-{running,success,failed,scheduled,pending}` (light only)** — re-valued from the Tailwind _500_ hues to 600/700. Measured on their own `-bg` tint in the running app, every one failed WCAG AA for the badge text they back: `success` 2.18:1, `scheduled` 2.07:1, `running` 3.38:1, `failed` 3.44:1, `pending` 4.39:1. They were chosen as dot/bar fills and only later reused as text on a tint, which is where they break down.

  | token       | before    | after                 | on `-bg` |
  | ----------- | --------- | --------------------- | -------- |
  | `running`   | `#3b82f6` | `#2563eb` (blue-600)  | 4.75:1   |
  | `success`   | `#22c55e` | `#15803d` (green-700) | 4.79:1   |
  | `failed`    | `#ef4444` | `#b91c1c` (red-700)   | 5.91:1   |
  | `scheduled` | `#f59e0b` | `#b45309` (amber-700) | 4.84:1   |
  | `pending`   | `#6b7280` | `#4b5563` (gray-600)  | 6.87:1   |

  Dark values are untouched — those are the 400 hues and already clear AA on a dark tint (4.52:1 – 8.73:1). The solid-fill usages (`stateColors.ts` dots/bars, `GenerativeUiBlocks` timeline segments) carry no text on top and read stronger on white at the darker value; no call site pairs these with white text.

### Removed — tokens

- **`--input-bg-hover`** — removed because text inputs and textareas now retain `--input-bg` on hover. Hover feedback remains on the border; select controls keep their existing hover background behavior.

---

## [1.3.0] - 2026-07-23

Narrows this file to the token layer and closes out the entries that accumulated under the previous scope.

The token section below is complete for the whole 1.2.0 → 1.3.0 window: most of it was reconstructed from `git log` on `theme.css` / `tailwind.config.js`, because six token groups were added and one re-valued without ever reaching this file.

### Added — tokens

- **`--color-ai-touched-fade`** — background tint that fades over 2s on a form control right after a successful AI generation. `color-mix(--color-primary-600 12%, transparent)` light / `18%` dark; the dark tint is stronger because the lighter primary needs more weight against slate-950. See [ADR-0066](../ADR/0066-inline-ai-post-generation-affordances.md).
- **`--color-connector-vector` / `-bg` / `-text`** — Qdrant vector connector identity. `#d946ef` / `#fdf4ff` / `#86198f` light, `#e879f9` / `rgba(217,70,239,.15)` / `#f5d0fe` dark. PR #573.
- **`--color-schema-type-{text,number,temporal,boolean,complex,binary}`** — schema editor column-type identity. Light `#2563eb` / `#0d9488` / `#d97706` / `#7c3aed` / `#db2777` / `#64748b`; dark lifts each to its 300–400 step. See [ADR-0139](../ADR/0139-schema-editor-modern-redesign-and-unification.md).
- **`--color-border-strong`** — emphasized hover/active border, one step past `--color-border`. `#94a3b8` light / `#475569` dark, mapped to Tailwind `border-strong`. Added because `border-strong` classes were already in use with no token behind them. PR #667.
- **Tailwind mappings for existing tokens** — `status-{running,success,failed,scheduled,pending}-bg`, `warning-600`, `warning-50`, `modal-footer`, `modal-border`, `modal-muted`. The CSS variables existed; the utility classes referencing them did not resolve. PR #678.
- **`borderColor.DEFAULT` → `var(--color-border)`** — Tailwind preflight resolves `border-color: theme('borderColor.DEFAULT', currentColor)` for every element, so without a DEFAULT any unresolved border fell back to `currentColor`, which reads as a bright line in dark mode. Setting it closes that failure mode globally rather than per component. See [ADR-0144](../ADR/0144-default-border-color-dark-mode-currentcolor-fix.md), PR #681.
- **`--color-entity-transform`** — dedicated identity for transform workflow nodes, which previously borrowed another entity's color. `#be185d` light / `#f472b6` dark. PR #984.
- **`--color-code-{keyword,string,number,comment,title,attr}`** — chat code-block syntax highlighting, mapped to `highlight.js` classes in `styles/components/chat-code.css`. See [ADR-0204](../ADR/0204-chat-surface-message-ux-standardization.md).

### Changed — tokens

- **Dark-mode text / tint contrast (WCAG AA)** — `--color-primary-container` `#2a3575 → #3a44a0` (boxed icon vs slate-800 card `1.30 → 1.75:1`); `--color-text-tertiary` and `--input-placeholder` `#64748b → #8a96ad` (card `3.0 → 4.9:1`, AA). The slate palette itself was already healthy — this is a localized correction, not a re-theme. See [ADR-0147](../ADR/0147-dark-mode-form-card-icon-and-contrast.md), PR #695.
- **Light-mode text-tier contrast (WCAG AA)** — `--color-text-secondary` and `--color-card-text-secondary` `#6b7280 → #4b5563` (`7.55:1`); `--color-text-tertiary` `#9ca3af → #6b7280` (`2.54 → 4.57:1`, AA). White has less contrast headroom than the slate-950 canvas, so the light tiers step down one notch rather than mirroring dark; tertiary is the lightest _readable_ tier (essential small copy uses secondary). Resolves BACKLOG S-016. See [ADR-0148](../ADR/0148-light-mode-text-tier-contrast.md), PR #699.
- **`--color-panel` (light)** — `#eef3f9 → #f3f4f6`. The blue-tinted inset clashed with the neutral app chrome; gray-100 harmonizes with it. See [ADR-0182](../ADR/0182-editor-panel-neutral-chrome-harmony.md), PR #945.
- **Docs synced to the token values** — `.impeccable.md`, `COLOR_SYSTEM.md`, `ACCESSIBILITY.md`, `COMPONENTS.md`, `PATTERNS.md` (PRs #701 / #707 / #718), then `DESIGN.md` and `tokens.json`, which that pass missed and which still carried pre-ADR-0147/0148 values plus a stale "runtime-customizable primary" claim — the product primary is fixed. PR #1201.

### Recorded under the previous scope

These shipped in this window and are kept for continuity. Under the scope above they would now live in `COMPONENTS.md` / `PATTERNS.md` and their ADRs, not here.

- **`InputParameterEditor`** — shared, container-responsive input-definition editor for Tool, Actor, workflow Start variables, and Human Input fields. Wide surfaces use a persistent table header; narrow inspectors use labeled stacked fields. Adds visible row actions, accessible control names, multiline descriptions, and empty/duplicate-name validation while preserving each domain's existing data contract.
- **Restrained selection surfaces** — `PATTERNS.md §1.7` documents how selected rows, cards, segmented controls, provider tiles, and capability lists use low-intensity primary tint plus softened borders instead of full-strength container fills; the global design principles reserve strong primary for compact signals such as checkbox fill, focus rings, active underlines, and CTA fill.
- **Boxed form-card icons** — `SectionCard` default variant promoted `'form' → 'accent'`, so all 19 resource-edit forms render section-scoped semantic boxed title icons (left accent line dropped to keep the surface restrained). Opt into the minimal boxless glyph with `variant="form"`. See [ADR-0147](../ADR/0147-dark-mode-form-card-icon-and-contrast.md), PR #695.
- **`SegmentedControl` form pattern + mono technical inputs** — the binary / immutable resource-type field (value in a hidden `Form.Item`, `SegmentedControl` on create, disabled `Input` on edit, preferring `model?.type` over the watched value to avoid a first-render flicker), and `font-mono` on machine-parsed inputs including the AntD `InputNumber` caveat that requires `[&_input]:!font-mono`. See [ADR-0151](../ADR/0151-model-form-type-segmented-and-mono-inputs.md), PRs #712 / #718 / #720 / #742.

---

## [1.2.0] - 2026-05-18

List page load error state — shared `ListLoadError` component plus `error` variant on `ListEmptyState` so all 12 list pages surface fetch failures with the same icon, tone, and copy. See [ADR-0057](../ADR/0057-list-load-error-shared-state.md).

### Added

- **`ListLoadError`** component (`src/components/ListLoadError.tsx`) — view wrapper that combines `classifyListLoadError` with `ListEmptyState`'s new `error` variant. Props: `error`, `resource`, `onRetry`, `forceKind`.
- **`classifyListLoadError(error)`** helper (`src/components/classifyListLoadError.ts`) — pure classifier buckets errors into `network` / `forbidden` / `server` / `generic` using `ApiError.status` when available.
- **`'error'` variant** on `ListEmptyState` — same dashed border + `bg-empty-state` shell as the `initial` / `foundation` variants so empty and failed states read as the same family.
- **`common.listErrorState.{network,forbidden,server,generic}.{title,description}`** i18n keys (ko/en) with `{{resource}}` and `{{status}}` interpolation. Each list page now contributes a single `xxx.list.resourceName` key (12 keys per locale total).
- **`resolveMockErrorResponse()`** helper (`src/mocks/handlers/utils.ts`) and `?mockError=` URL toggle on the relevant MSW handlers — instant visual regression coverage for all four error kinds in mock mode without touching the backend.

### Changed

- **Decision Matrix in `COMPONENTS.md`** — added a "List load error" row mapping to the shared component.
- **`PATTERNS.md` §5.3 List Load Error State** — new section documenting classifier, visual rules, i18n, branch order, multi-query / enrichment handling, and MSW review.
- **Dashboard list page** — migrated from inline red-text error + `dashboard.list.fetchError` single key to the shared component + common keys (Phase A, PR #183).
- **Ontology, Labs/Reports, Workflows, Users, Groups, Connectors, Recents** — migrated to the shared component (Phase B, PR #192). Recents uses `forceKind="generic"` and only surfaces the error when all 7 of its aggregated queries fail.
- **Agents, Tools, Actors, Knowledge** — migrated to the shared component (Phase B+, PR #193). Knowledge's enrichment queries (`useKnowledgeStats`, `useKnowledgeStatusSummaries`) stay outside the error branch so partial enrichment failure does not block the main list.

### Removed

- **Per-page `errorState` i18n keys** — `dashboard.list.fetchError`, `pages.ontology.list.fetchError` removed in favour of the common key set.

---

## [1.1.0] - 2026-04-20

Primary-container migration — introduce the MD3 container / on-container role pair for tinted surfaces, retire three legacy tokens whose dark-mode values were semantically inverted, and fix a dark-mode button hover regression.

### Added

- **`--color-primary-container`** / **`--color-on-primary-container`** tokens in `theme.css` (both `:root` and `[data-theme='dark']`). Light: `#e0e7ff` / `#1e1b4b`. Dark: `#2a3575` / `#c7d2fe`. Contrast ≥ 6.7:1 in both themes. Use for chips, active tabs, info boxes, hover tints — anywhere a tinted primary surface needs readable foreground text.
- **Tailwind mappings** for `primary-container` and `on-primary-container` in `tailwind.config.js`.
- **Per-entity hero accents** — `.entity-detail--pipeline` and `.entity-detail--knowledge` variants wired to the existing `--color-entity-*` palette.

### Changed

- **`.entity-detail` hero** now binds `--entity-accent` to per-entity tokens (`--color-entity-{collection,dataset,code,pipeline,knowledge}`) instead of `--color-primary-600` / `--color-accent-600`. The existing `color-mix` pill derivation works correctly in both themes because the entity tokens are already light/dark-inverted.
- **`.dashboard-card-btn-primary:hover`** — was `background-color: --color-primary-700`, which inverted to a _lighter_ indigo in dark mode (regression). Now `color-mix(--color-primary-600 85%, black)` — darkens consistently across themes.
- **Form `<label>` color** — migrated from `--color-primary-700` to `--color-heading` for neutral consistency.
- **`a:hover` link color** — migrated from `--color-primary-700` to `--color-on-primary-container`.
- **19 call sites** across components and CSS migrated from `bg-primary/N text-primary` / `--color-primary-50` / `--color-primary-700` to the container pair. Full list in each PR description (#79, #80, #83, #84, #86).

### Removed

- **`--color-primary-50`** — legacy token whose dark value was `#1e293b` (slate-800, not a "primary tint"). All consumers migrated to `--color-primary-container`.
- **`--color-primary-700`** — legacy token whose dark value was `#e0e7ff` (indigo-100), inverted from its "deepest primary shade" semantic. Migrated per-call-site to `--color-on-primary-container` or `--color-heading`.
- **`--color-chip-bg`** — near-duplicate of `--color-primary-container` after migration. Removed from `theme.css`, `tailwind.config.js`, and preview HTMLs.
- **Undefined token references** — `--color-primary-500` (10 sites), `--color-primary-100` (1 site), `--color-primary-900` (1 stale comment) were referenced but never defined in `theme.css`; `var()` was silently falling back to property initial values. All replaced with defined tokens.
- **`generateCSSVariables()` in `themeColors.ts`** no longer emits `--color-primary-50` / `--color-primary-700` keys; test array trimmed.

### Fixed

- **Dark-mode invisible text in workflow Inspector** — `.dark .workflow-inspector` had `--inspector-message-text: var(--color-primary-50)`, which resolved to `#1e293b` (slate-800) on a dark surface. Text was practically invisible. Inspector tokens now use the container pair.
- **Dark-mode dashboard button hover inversion** — see Changed above.

### Docs

- **COLOR_SYSTEM.md**: Brand Identity table replaced legacy "Primary 50 / 700" rows with "Primary container / On primary container".
- **MIGRATION_GUIDE.md**: stale legacy-token recommendations removed, anti-pattern `bg-primary/N text-primary` added.
- **PATTERNS.md**: new "Tinted surface pattern" section covering container role usage and the dark-mode hover gotcha.
- **COMPONENTS.md**: AppTag `label` variant note updated to reference container pair.
- **tokens.json**: container tokens added, legacy entries removed.

---

## [1.0.0] - 2026-03-30

### Added

- **COLOR_SYSTEM.md**: Complete color token reference with 100+ CSS variables, light/dark values, 5-layer architecture documentation, and dynamic primary color system.
- **tokens.json**: W3C DTCG format machine-readable design tokens covering color, spacing, typography, motion, layout, workflow, and icon sizing.
- **COMPONENTS.md**: Component catalog documenting 18 UI primitives (shadcn/ui) and 31 shared components with props, variants, and usage examples. Includes component decision matrix (shadcn vs Ant Design).
- **PATTERNS.md**: Usage pattern guide covering dark mode (3-tier priority), spacing scale, typography system, motion tokens, form patterns, inline style rules, and contribution checklist.
- **ACCESSIBILITY.md**: WCAG 2.1 AA checklist, color contrast requirements, component-specific a11y rules (Button, Modal, Form, Table, Tree, Tabs), keyboard navigation guide, screen reader utilities, and reduced motion support.
- **MIGRATION_GUIDE.md**: Hardcoded hex-to-token mapping table, 5 anti-pattern replacement guides, file priority list (HIGH/MEDIUM/LOW), and quick win suggestions.
- **CHANGELOG.md**: This file.

### Changed

- **tailwind.config.js**: Added 11 semantic color mappings (`surface`, `surface-muted`, `elevated`, `subtle`, `chrome`, `heading`, `text-primary`, `text-secondary`, `text-tertiary`, `border-default`, `border-card`) mapped to CSS custom properties.
- **AGENTS.md**: Added Design System section with 5-layer token architecture, color token rules, dark mode priority, component selection matrix, and design system doc references. Enhanced DO/DON'T items #5, #1, and added #11 to both lists.
- **package.json**: Enhanced `lint:colors` script to exclude known-acceptable token source files (`theme.css`, `entityColors.ts`, `chartColors.ts`, `themeColors.ts`).

### Infrastructure

- **.cursor/rules/design-system-rules/RULE.md**: New Cursor rule for automatic design system enforcement during collaborative coding. Includes token quick reference tables, entity color API guide, component selection matrix, and deep reference links.
