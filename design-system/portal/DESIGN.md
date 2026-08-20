# Design System: D.Hub2 Portal

## 1. Visual Theme & Atmosphere

D.Hub2 Portal is a data platform management dashboard built for enterprise teams managing datasets, pipelines, ontologies, and knowledge bases. The design is a clean, structured environment that balances dense data presentation with breathing room — a digital workspace for data engineering, not a consumer product.

The foundation is a cool-neutral canvas (`#f8fafc` container, `#ffffff` surfaces) with a deep indigo brand accent (`#3b50ce`) that conveys technical authority without coldness. The palette is built on the Tailwind Slate scale, providing 11 precisely stepped grays for surface hierarchy. Dark mode inverts onto `#020617` (Slate 950) with `#1e293b` surfaces, achieving a premium night-shift aesthetic.

Typography uses **Inter** (400–600) for body text and UI, **Roboto** (700) for section headings, and **Fira Code** (400) for code blocks. The type scale runs from 11px (badges) to 30px (page titles), with tight negative letter-spacing on headings (-0.02em to -0.01em) and relaxed line-heights (1.5) for body content.

What distinguishes D.Hub2 is its **entity-color system** — six distinct 4-shade palettes (Collection/Dataset/Code/Pipeline/Document/Knowledge) that provide instant visual identification across badges, node cards, and tree panels. Combined with a **workflow node category system** (Flow/AI/Data/Integration/Control/Advanced), the interface uses color semantically rather than decoratively.

**Key Characteristics:**

- Cool-neutral canvas with deep indigo brand accent (`#3b50ce`)
- 5-layer color architecture: Ant Design → CSS Custom Properties → shadcn/ui HSL → Tailwind → TypeScript constants
- Fixed product primary — light/dark tokens in `theme.css`, no runtime JS injection
- Entity-color system: 6 entity types × 4 shade stops for instant visual identification
- Workflow node categories with stroke/bg/text triples
- 8px default border radius — consistent across all containers
- Gradient accent buttons with glow effect for primary CTAs
- Slate-based dark mode with 11-step tonal hierarchy

## 2. Color Palette & Roles

### Brand Identity

- **Primary** (`#3b50ce`): Brand color, Ant Design `colorPrimary`, focus rings, input focus border
- **Primary 600** (`#3b50ce` light / `#3b82f6` dark): Role A — solid fills (Button bg, focused border, link base)
- **Primary container** (`#e0e7ff` light / `#3a44a0` dark): Role B — tinted soft surface for chips, tags, active tabs, info boxes
- **On primary container** (`#1e1b4b` light / `#c7d2fe` dark): Readable text/icon on primary container (WCAG AA in both themes)

### Text

- **Heading** (`#1f2937` light / `#f1f5f9` dark): Section headings
- **Primary** (`#1f2937` light / `#f8fafc` dark): Main body text
- **Secondary** (`#4b5563` light / `#94a3b8` dark): Supporting descriptions
- **Tertiary** (`#6b7280` light / `#8a96ad` dark): Placeholder, hint text
- **Muted** (`#64748b` light / `#94a3b8` dark): Helper text, timestamps

### Surface & Background

- **Surface** (`#ffffff` light / `#1e293b` dark): Card/container surface
- **Panel** (`#f3f4f6` light / `#0f172a` dark): Panel/sidebar background
- **Container** (`#f8fafc` light / `#020617` dark): App container
- **Chrome** (`#f7f8fa` light / `#0f172a` dark): Sidebar, navbar chrome
- **Elevated** (`#ffffff` light / `#1e293b` dark): Elevated widgets
- **Subtle** (`#f1f5f9` light / `#334155` dark): Subtle backgrounds

### Border

- **Default** (`#d4dbe7` light / `#334155` dark): General borders
- **Card** (`#e2e8f0` light / `#334155` dark): Card borders

### Status

- **Info**: `#3b50ce` / `#f0f5ff` (light) — `#60a5fa` / `#1e3a5f` (dark)
- **Success**: `#16a34a` / `#f0fdf4` (light) — `#4ade80` / `#1a3a2e` (dark)
- **Warning**: `#f59e0b` / `#fffbeb` (light) — `#fbbf24` / `#3a2e1a` (dark)
- **Danger**: `#dc2626` / `#fef2f2` (light) — `#f87171` / `#3a1a1a` (dark)

### Pipeline Execution Status

- **Running**: `#3b82f6` / `#eff6ff` (light) — `#60a5fa` / `#1e3a5f` (dark)
- **Success**: `#22c55e` / `#f0fdf4` (light) — `#4ade80` / `#14532d` (dark)
- **Failed**: `#ef4444` / `#fef2f2` (light) — `#f87171` / `#450a0a` (dark)
- **Scheduled**: `#f59e0b` / `#fffbeb` (light) — `#fbbf24` / `#422006` (dark)
- **Pending**: `#6b7280` / `#f3f4f6` (light) — `#94a3b8` / `#1e293b` (dark)

### Entity Type Colors (100 / 300 / 600 / 900)

- **Collection**: `#e2e8f0` / `#cbd5e1` / `#475569` / `#0f172a`
- **Dataset**: `#E6ECF3` / `#8DA8CC` / `#5E7BAE` / `#2A3A50`
- **Code**: `#F2E8E0` / `#D1A882` / `#B87F56` / `#4A3425`
- **Pipeline**: `#E2F0EA` / `#7DB8A2` / `#52917A` / `#263E34`
- **Document**: `#EBE5F2` / `#A893C7` / `#7E63A5` / `#322546`
- **Knowledge**: `#ECE5F3` / `#AD96CC` / `#876BAF` / `#352849`

### Chart Categorical (10 colors)

- Muted Blue `rgba(102,136,187,0.85)`, Muted Teal `rgba(85,170,170,0.85)`, Muted Amber `rgba(221,170,68,0.85)`, Muted Coral `rgba(217,123,123,0.85)`, Muted Green `rgba(119,187,136,0.85)`, Muted Slate `rgba(136,153,170,0.85)`, Muted Pink `rgba(204,136,153,0.85)`, Light Blue `rgba(153,187,221,0.85)`, Muted Orange `rgba(232,166,69,0.85)`, Gray `rgba(184,184,184,0.85)`

### Workflow Node Categories (stroke / bg / text)

- **Flow**: `#6366f1` / `#eef2ff` / `#3730a3` (Indigo)
- **AI**: `#8b5cf6` / `#f5f3ff` / `#5b21b6` (Violet)
- **Data**: `#0ea5e9` / `#f0f9ff` / `#0369a1` (Sky)
- **Integration**: `#f59e0b` / `#fffbeb` / `#92400e` (Amber)
- **Control**: `#10b981` / `#ecfdf5` / `#065f46` (Emerald)
- **Advanced**: `#ef4444` / `#fef2f2` / `#991b1b` (Red)

## 3. Typography Rules

### Font Family

- **Body**: `Inter`, fallbacks: `-apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Helvetica Neue, sans-serif`
- **Headings**: `Roboto`, same fallbacks
- **Code**: `Fira Code`, fallbacks: `Consolas, Monaco, Courier New, monospace`

### Hierarchy

| Role            | Font      | Size        | Weight | Line Height | Letter Spacing | Notes                      |
| --------------- | --------- | ----------- | ------ | ----------- | -------------- | -------------------------- |
| Page Title      | Roboto    | 30px (4xl)  | 700    | 1.2         | -0.02em        | Dashboard, page headers    |
| Section Heading | Roboto    | 24px (3xl)  | 700    | 1.2         | -0.02em        | Section titles             |
| Sub-heading     | Inter     | 20px (2xl)  | 600    | 1.35        | -0.01em        | Card group titles          |
| Card Title      | Inter     | 18px (xl)   | 600    | 1.35        | -0.01em        | Card headers               |
| Label           | Inter     | 16px (lg)   | 500    | 1.5         | 0              | Nav items, emphasized body |
| Body            | Inter     | 14px (base) | 400    | 1.5         | 0              | Standard body text         |
| Compact         | Inter     | 13px (md)   | 400    | 1.5         | 0              | Dense UI areas             |
| Small           | Inter     | 12px (sm)   | 400    | 1.5         | 0              | Helpers, timestamps        |
| Micro           | Inter     | 11px (xs)   | 400    | 1.5         | 0              | Badge labels, fine print   |
| Code            | Fira Code | 14px        | 400    | 1.5         | 0              | Code blocks, monospace     |

### Principles

- **Inter dominance**: 400–600 weight range covers all UI text. No weight below 400.
- **Roboto for authority**: Reserved exclusively for page/section headings at 700 weight.
- **Tight headings**: -0.02em to -0.01em letter-spacing on titles for compact, confident feel.
- **Comfortable body**: 1.5 line-height for all body text — data-dense UIs need reading comfort.

## 4. Component Stylings

### Buttons

**Primary (Gradient)**

- Background: `linear-gradient(#3b50ce, #2837a8)`
- Text: `#ffffff`
- Radius: 8px
- Hover: `linear-gradient(#2837a8, #192182)`
- Glow: `color-mix(in srgb, var(--color-primary-600) 30%, transparent)`

**Secondary**

- Background: `#f5f5f5` (light) / `#1e293b` (dark)
- Text: `#171717` (light) / `#f8fafc` (dark)
- Radius: 8px

**Ghost**

- Background: transparent
- Text: `#1f2937` (light) / `#f8fafc` (dark)
- Hover: `#f1f5f9` (light) / `#334155` (dark)
- Radius: 8px

**Outline**

- Border: `#d4dbe7` (light) / `#334155` (dark)
- Text: `#1f2937` (light) / `#f8fafc` (dark)
- Radius: 8px

**Destructive**

- Background: `#dc2626` (light) / `#f87171` (dark)
- Text: `#ffffff`
- Radius: 8px

### Form Inputs

- Default: bg white, border `#d1d5db`, text `#1f2937`, placeholder `#6b7280`
- Focus: border `var(--color-primary-600)`, ring `rgba(59, 80, 206, 0.2)`
- Error: border `#dc2626`, ring `rgba(220, 38, 38, 0.2)`
- Disabled: bg `#f3f4f6`, border `#e5e7eb`, text `#6b7280`
- Radius: 8px

### Cards

- Background: `#ffffff` (light) / `#1e293b` (dark)
- Border: `#e2e8f0` (light) / `#334155` (dark)
- Radius: 8px
- Shadow: `0 1px 2px rgba(15, 23, 42, 0.06)` (sm)
- Selected: bg `#e6f7ff`, border `#3b50ce`
- Hover: shadow-md, subtle lift

### Alerts

- Left border: 4px solid status color
- Background: status bg color
- Text: status fg color
- Radius: 8px

### Entity Badges (Pill)

Light mode: entity-600 text on entity-100 bg. Dark mode: entity-300 text on entity-900 bg.

### Status Badges (Pill)

Foreground on background per status table above.

## 5. Layout Principles

### Spacing Scale

- Base unit: 4px
- Scale: 2px, 4px, 8px, 12px, 16px, 24px, 32px, 48px, 64px, 96px

### Layout Dimensions

- Navbar height: 50px
- Sidebar expanded: 240px
- Sidebar collapsed: 60px
- Content max-width: 1400px
- Panel width: 320px

### Whitespace Philosophy

- **Enterprise breathing room**: 24px padding on main content areas. Dense data with adequate spacing.
- **Section separation**: 32–48px between major sections.
- **Card grids**: 16px gaps — tight enough for scanning, loose enough for clarity.

### Border Radius Scale

- Small (4px): Inputs, small elements
- Medium (6px): Tags, pills
- Large (8px): Cards, containers — the default
- Node (12px): Workflow nodes

## 6. Depth & Elevation

| Level              | Shadow                                 | Use                          |
| ------------------ | -------------------------------------- | ---------------------------- |
| Flat (Level 0)     | None                                   | Page background, text blocks |
| Subtle (Level 1)   | `0 1px 2px rgba(15, 23, 42, 0.06)`     | Cards at rest, inputs        |
| Default (Level 2)  | `0 8px 20px rgba(15, 23, 42, 0.12)`    | Cards on hover, dropdowns    |
| Elevated (Level 3) | `0 18px 36px rgba(148, 163, 184, 0.2)` | Modals, popovers             |

**Shadow Philosophy**: Shadows use Slate-tinted rgba values rather than pure black, creating a cool, integrated depth effect that matches the overall palette. The three-tier system (sm/md/lg) provides clear hierarchy without visual noise.

## 7. Do's and Don'ts

### Do

- Use `#1f2937` for primary text — never pure `#000000`
- Apply `#3b50ce` as the brand accent for CTAs and focus states
- Use the entity color system (6 types × 4 shades) for all entity-related UI
- Apply 8px border radius consistently across all containers and cards
- Use CSS custom properties (`var(--color-*)`) — never hardcode hex in components
- Use gradient buttons for primary CTAs — the glow adds premium feel
- Use `Inter` at 400–600 for body, `Roboto` 700 for headings only
- Reference `docs/design-system/COLOR_SYSTEM.md` for exact token values

### Don't

- Don't hardcode color hex values in TSX/CSS — use tokens exclusively
- Don't use `#000000` for text — always Slate-based dark colors
- Don't mix entity color palettes — each entity type has its own 4-shade set
- Don't use heavy shadows (>0.2 opacity) — keep them cool and subtle
- Don't use sharp corners (0–2px) on cards — 8px minimum for containers
- Don't introduce ad-hoc colors outside the design token system
- Don't use `style={{}}` for colors — use Tailwind classes or CSS variables
- Don't skip dark mode — every token has a light/dark pair

## 8. Responsive Behavior

### Breakpoints

| Name    | Width       | Key Changes                             |
| ------- | ----------- | --------------------------------------- |
| Mobile  | <768px      | Sidebar hidden, single column           |
| Tablet  | 768–1024px  | Sidebar collapsed (60px), 2-column grid |
| Desktop | 1024–1440px | Sidebar expanded (240px), multi-column  |
| Large   | >1440px     | Content max-width 1400px, centered      |

### Collapsing Strategy

- Sidebar: 240px → 60px (icon-only) → hidden (mobile)
- Content: fills available width up to 1400px max
- Cards: multi-column → 2-column → single column
- Tables: horizontal scroll on narrow viewports
- Panel: overlay on mobile, side panel on desktop

## 9. Agent Prompt Guide

### Quick Color Reference

- Background: `#f8fafc` (container), `#ffffff` (surface)
- Text: `#1f2937` (primary), `#4b5563` (secondary)
- Brand accent: `#3b50ce` (primary), `#192182` (hover)
- Border: `#d4dbe7` (default), `#e2e8f0` (card)
- Card shadow: `0 1px 2px rgba(15, 23, 42, 0.06)`

### Example Component Prompts

- "Create a dataset card: white background, `#e2e8f0` border, 8px radius, shadow-sm. Entity badge: `#5E7BAE` text on `#E6ECF3` bg. Status pill: `#22c55e` text on `#f0fdf4`. Title: 16px Inter 600, description: 14px Inter 400 `#4b5563`."
- "Design the sidebar: 240px width, `#f3f4f6` bg. Active item: `#3b50ce` left accent bar, `#f0f5ff` bg. Inactive: `#64748b` text, hover `#f1f5f9`. Nav items: 14px Inter 500."
- "Build a pipeline node card: `var(--color-card-bg)` bg, 272px width, 8px radius, BEM `.pl-node-card` classes. Gradient accent bar via `--wf-node-color`. 28×28 gradient icon container, 13px title."
- "Create a stats card: white bg, `#e2e8f0` border, 8px radius, shadow-sm. Label: 12px Inter 400 `#64748b`. Value: 24px Roboto 700 `#1f2937`."
- "Design an alert: 4px left border. Info variant: `#3b50ce` border, `#f0f5ff` bg, `#3b50ce` icon, `#1f2937` text. 8px radius."

### Iteration Guide

1. Start with `#f8fafc` container background — the Slate-50 canvas
2. `#3b50ce` is the singular brand accent — gradient for CTAs, solid for focus/links
3. 8px radius is the DNA — apply to all cards, inputs, buttons
4. Entity badges use the 6-type color system — never improvise entity colors
5. Shadows are cool-tinted (Slate rgba) — never use pure black shadows
6. Dark mode inverts onto Slate-950 (`#020617`) — every token has a dark pair
7. Inter 400–600 for everything except headings — Roboto 700 for headings only
