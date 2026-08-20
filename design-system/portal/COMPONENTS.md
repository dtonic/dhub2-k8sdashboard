# Component Catalog

Comprehensive reference for all reusable components in D.Hub2 Portal. Before creating a new component, check this catalog first.

**Total**: 18 UI primitives (`src/components/ui/`) + 74 shared components at the root of `src/components/`, plus 23 grouped subdirectories.

**This file is the source of truth for what exists.** A new shared component belongs here, in the Decision Matrix if it is a choice someone could get wrong and in the catalog either way. The matrix in `AGENTS.md` is a short list of the most common decisions and links back here; adding a component only there leaves it undiscoverable from the catalog, which is how `SecretRefField`, `AskAssistantButton`, and `KnowledgeDeleteConflictModal` went missing from this page while being required reading in `AGENTS.md`.

---

## Component Decision Matrix

Use this table to decide which library to use for a given UI need.

| UI Need                        | Use                                                                                    | Why                                                                                |
| ------------------------------ | -------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| Button (all variants)          | shadcn/ui `Button`                                                                     | CVA variants, consistent sizing, loading state                                     |
| Text input                     | shadcn/ui `Input`                                                                      | Themed Ant wrapper with error states                                               |
| Multi-line text                | shadcn/ui `Textarea`                                                                   | Themed Ant wrapper with resize control                                             |
| Select dropdown                | shadcn/ui `Select`                                                                     | Themed Ant wrapper with error states                                               |
| Checkbox / Radio / Switch      | shadcn/ui                                                                              | Themed Ant wrapper with inspector/modal variants                                   |
| Form field + label             | shadcn/ui `FormField` + `FormLabel`                                                    | Compound layout with error/description slots                                       |
| Resource basic info form       | Shared `ResourceBasicInfo` / `ResourceBasicInfoSection`                                | Standard name/alias/collection/description/tags block for create/edit              |
| Persisted record metadata      | Shared `ResourceRecordInfoSection`                                                     | Copyable ID, owner, created, and updated fields in a stable order                  |
| Resource create/edit layout    | Shared `ResourceFormLayout`                                                            | Semantic primary/rail/full-width placement for resource form sections              |
| Search input                   | shadcn/ui `SearchInput` or `FilterInput`                                               | Themed with icon prefix                                                            |
| List toolbar                   | Shared `ListToolbar` + `ListSearchInput`                                               | Standard list-page filter/search strip                                             |
| List segmented filter          | Shared `ListSegmentedFilter`                                                           | Status/type/provider filters with optional counts                                  |
| Managed list table             | Shared `DataTable` + `@components/table` column helpers                                | Resource lists with skeleton, row click, pagination, URL-persisted header state    |
| Status badge                   | shadcn/ui `StatusBadge`                                                                | Pipeline/batch status with auto-coloring                                           |
| Page/section loading           | `TableSkeleton` (lists) · `antd` `Skeleton` (panels) · `CollectionTreeSkeleton` (tree) | Reveals layout structure — never use `Spin` for cold loads                         |
| Inline loading affordance      | shadcn/ui `Spinner` / `InlineSpinner`, `IconLoader2`                                   | Inside a button, chip, or small inline status                                      |
| Inline AI generate trigger     | Shared `InlineAIField` + `AIGenerateButton`                                            | Sparkle inside form fields, brand-rainbow sweep on loading (ADR-0065)              |
| AI-generation content wait     | Shared `AISkeleton`                                                                    | Brand-rainbow shimmer for incoming model output; not for data loads (PATTERNS §10) |
| Functional AI action icon      | Shared `AITriggerIcon`                                                                 | Same two-layer sparkle used by inline AI triggers for small AI actions             |
| Editor-style tabs              | Shared `EditorTabs`                                                                    | ADR-0093 Type B resource editors with separate editing contexts                    |
| Full-page form rhythm          | Shared `FormWorkspace` + `ResourceFormLayout` + `SectionCard`                          | ADR-0093 shell, semantic section placement, card rhythm                            |
| Page / detail / builder header | Shared `WorkspaceHeader` (or `FormWorkspace`, which renders it)                        | Single source for page headers; never hand-roll                                    |
| Page / list header icon box    | Shared `PageHeaderIcon` (or `WorkspaceHeader`/`FormWorkspace` `pageKey`)               | ADR-0158 2-tier tone: resource tint / meta neutral; no gradient boxes              |
| List page header shell         | Shared `ListPageHeader`                                                                | ADR-0236 sticky shell: border/title tokens, icon alignment, action slot            |
| Modal header (create/edit)     | Shared `ModalTitle variant="form"`                                                     | Mirrors `WorkspaceHeader` so the action looks identical page ↔ modal               |
| Modal header (confirm/info)    | Shared `ModalTitle` (default variant)                                                  | Lighter title for confirm/delete/help/info dialogs                                 |
| Modal / form footer actions    | Shared `ModalActions`                                                                  | Cancel/confirm row: order, ghost cancel, sizing, loading, destructive              |
| Stats dashboard card           | shadcn/ui `StatsCard`                                                                  | Trend indicator, loading skeleton                                                  |
| List empty state               | Shared `ListEmptyState`                                                                | Standardized widget/initial/filtered empty state                                   |
| List load error                | Shared `ListLoadError`                                                                 | Classified fetch error (network/forbidden/server/generic)                          |
| Raw data / preview table       | **Ant Design** `Table`                                                                 | Dynamic result previews, pickers, or non-managed tables                            |
| Tree view                      | **Ant Design** `Tree`                                                                  | Expand/collapse, drag-and-drop                                                     |
| Modal / Dialog                 | **Ant Design** `Modal`                                                                 | Focus trap, footer actions                                                         |
| Complex form                   | **Ant Design** `Form`                                                                  | Validation rules, field dependencies                                               |
| Tabs                           | **Ant Design** `Tabs`                                                                  | Lazy rendering, closable tabs                                                      |
| Dropdown menu                  | **Ant Design** `Dropdown`                                                              | Contextual actions                                                                 |
| Page layout                    | **Ant Design** `Layout`                                                                | Sider + Content + Header                                                           |
| Card container                 | **Ant Design** `Card`                                                                  | Header/body/actions slots                                                          |
| List card (flat/compact)       | Shared `FlatCard`                                                                      | Accent bar, `<button>`, hover lift                                                 |
| Card skeleton loading          | Shared `CardSkeletonLoader`                                                            | 3 variants: preview, compact, flat                                                 |
| Delete confirmation            | Shared `DeleteModal`                                                                   | Name-typing verification pattern                                                   |
| Entity type icon               | Shared `EntityIcon` / `CategoryIcon`                                                   | Consistent entity colors                                                           |
| Collection selector            | Shared `CollectionSelect`                                                              | With inline creation                                                               |
| Collection picker dropdown     | Shared `CollectionPickerDropdown`                                                      | Searchable controlled collection picker used by global scope                       |
| Tag input                      | Shared `TagInput`                                                                      | Enter-to-add with removable pills                                                  |
| Empty state layout             | Shared `ListEmptyState`                                                                | Empty lists, widgets, filtered states                                              |
| User / owner identity          | Shared `SubjectIdentity` + `SubjectAvatar`                                             | Avatar + display name for users/groups/public                                      |
| Resource owner column (table)  | Shared `OwnerCell` + `OwnerPreview`                                                    | Owner avatar/name + hover preview popover, sized for list-page density             |
| Resource owner (non-table)     | Shared `OwnerBadge`                                                                    | Owner chips with `+N` collapse for detail heroes, viewers, cards                   |
| Resource type icon             | Shared `ResourceIcon`                                                                  | Any `ResourceType` incl. entity/relation/agent/tool/actor, optional tinted box     |
| Core entity type icon          | Shared `EntityIcon` / `CategoryIcon`                                                   | The six core `EntityType`s only                                                    |
| Ontology entity / relation ref | Shared `EntitySelect` / `RelationSelect`                                               | Collection-scoped ID pickers; mirror `DatasetSelect` for swappable step inspectors |
| Agent / connector reference    | Shared `AgentSelect` / `ConnectorSelector`                                             | Name-valued agent picker; type-grouped connector picker                            |
| Secret reference input         | Shared `SecretRefField` (`@components/SecretRefField`)                                 | Secret → key dependent selects emitting `secret://{id}/{key}` (ADR-0207)           |
| Connector credential input     | Shared `CredentialInputField`                                                          | Server-state-driven configured/unset credential field (ADR-0213)                   |
| Filter-bar dropdown            | Shared `MultiFilterDropdown` / `SingleFilterDropdown` (`@components/FilterDropdown`)   | Quiet outlined trigger, primary-wash when active; multi keeps the panel open       |
| Classification marking chip    | Shared `MarkingBadge`                                                                  | Mandatory markings as warning-token chips; renders nothing when empty (ADR-0154)   |
| Compact metadata chip          | Shared `CapabilityMetaTag` / `CapabilityMetaTags`                                      | Small neutral meta chips (collection, capability) with a shared wrapper            |
| Resource type tag              | Shared `PromptTypeTag` / `ToolTypeTag` / `SubtypeTag` family                           | One color per type, shared across list, builder, and preview surfaces              |
| Sample collection marker       | Shared `SampleBadge`                                                                   | Info-token identity marker for sample collections (ADR-0032)                       |
| JSON / code form field         | Shared `JsonFieldEditor`                                                               | Themed Monaco field; text state from the parent or an AntD `Form.Item` (ADR-0234)  |
| String map field               | Shared `KeyValueEditor`                                                                | Key-value rows for a `Record<string, string>`, not a JSON textarea (ADR-0234)      |
| Prompt variable declarations   | Shared `PromptVariablesEditor`                                                         | Add/remove rows for `PromptVariable[]`                                             |
| Reasoning effort selector      | Shared `ReasoningEffortControl`                                                        | Inherit pill + level pills; renders nothing for non-reasoning models (ADR-0134)    |
| Select option row (subject)    | Shared `SubjectOptionLabel`                                                            | Two-line avatar + name + email row for AntD `optionRender`                         |
| Menu item with description     | Shared `MenuLabelWithDescription`                                                      | Two-line dropdown/menu label                                                       |
| External LLM endpoint test     | Shared `LlmConnectionTest`                                                             | Presentational probe UI; caller injects `onTest` + `buildRequest`                  |
| User feedback (toast)          | `showXxxNotification()` from `@utils/notifications`                                    | Consistent dark/light styling, custom icons                                        |

For managed list table column ordering and header controls, especially collection
metadata placement and same-column filter behavior, follow
[PATTERNS.md §5.6](./PATTERNS.md#56-list-table-column-order).

## Managed List Table Helpers (`src/components/table/`)

Managed resource lists should start from the shared table helpers before adding page-local columns. This keeps row navigation, skeleton loading, pagination, header sort/filter affordances, and URL persistence aligned across menus.

| Helper                                                | Use for                                                                                                       |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `DataTable`                                           | Thin AntD `Table` wrapper with `TableSkeleton`, default pagination, row-click affordance, and URL table state |
| `createNameColumn`                                    | First identity column; alias-aware display, optional subtitle/icon, alphabetical sorter                       |
| `createCollectionColumnControls`                      | `컬렉션` / `Collection` column sorter + value filter from collection id and display labels                    |
| `createValueColumnFilter` / `createArrayColumnFilter` | Categorical header filters such as type, protocol, status, tags, and language                                 |
| `createOwnerColumn`                                   | Owner display via `OwnerCell`; pass `records` for a header value filter (ADR-0129 §2.3)                       |
| `createDateColumn`                                    | `Updated` / `Created` date display and chronological sorter                                                   |
| `createActionsColumn`                                 | Right-aligned row action menu column                                                                          |
| `useUrlTableState`                                    | URL-persisted header filters/sorters for managed lists that must use raw AntD `Table`                         |
| `useResizableColumns`                                 | Drag-to-resize column headers with per-user width storage; also owns `tableLayout` and `scroll.x` (ADR-0246)  |
| `ResizableHeaderCell`                                 | `components.header.cell` rendering the resize handle; not called directly                                     |

Header behavior is semantic, not page-local. The same column meaning must expose the same affordance in every managed list:

| Column meaning                         | Header affordance         | Preferred implementation                              |
| -------------------------------------- | ------------------------- | ----------------------------------------------------- |
| `이름` / `Name`                        | Sort                      | `createNameColumn`                                    |
| `수정일`, `생성일` / date              | Sort                      | `createDateColumn`                                    |
| `컬렉션` / `Collection`                | Sort + value filter       | `createCollectionColumnControls`                      |
| Type, protocol, status, tags, language | Value filter              | `createValueColumnFilter` / `createArrayColumnFilter` |
| Owner                                  | Value filter (multi)      | `createOwnerColumn` with `records` (ADR-0129 §2.3)    |
| Actions                                | No header control         | `createActionsColumn`                                 |
| Counts, aggregate values, URLs/runtime | No default header control | Document a domain exception before adding one         |

Every sortable/filterable managed-list column needs a stable `key`, because ADR-0165 stores header filters as `tf_<columnKey>` and sorting as `ts=<columnKey>:<order>` in the route URL. If a header control is omitted for an otherwise standard column, document the reason in the feature FSD or a tight code comment: async-only values, composite display, permission-dependent values, or domain-specific semantics are valid reasons.

---

## Resource Create/Edit Layout Taxonomy (ADR-0093)

Before implementing or refactoring a full-page resource create/edit screen, classify it into one of these layouts:

| Type | Layout                 | Use When                                                                                                                                   | Examples                                                                                                                |
| ---- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------- |
| A    | Single-flow card form  | The task creates/registers a new resource, is short, linear, or security-sensitive, and hiding fields behind tabs would slow the user down | Knowledge create, Connector create, Model create, LLM model create, Tool/Actor/Prompt/Secret create, OIDC client create |
| B    | Tabbed resource editor | The task views/edits an existing resource where metadata and runtime/configuration/permission work happen in different contexts            | Connector edit, Model edit, LLM model edit, Agent edit, Tool/Actor/Prompt edit, OIDC client edit, Secret edit           |
| C    | Special builder/canvas | The page is primarily a graph, canvas, workflow, or node inspector rather than a resource form                                             | Workflow builder, Ontology builder, Dashboard builder                                                                   |

Rules:

- Type B editors use the shared `EditorTabs` component and include at least `기본 정보 / Basic Info` and `구성 / Configuration`, unless a domain-specific tab such as `구현 / Implementation`, `미리보기 / Preview`, or `권한 / Permissions` is more precise.
- Type B edit headers use the edited resource's display name, not an action title. Compute it with `getResourceDisplayName(resource, getRouteResourceIdentifier(routeParam))` (`alias -> name -> decoded route identifier`) and use `getResourceNameSubtitle(resource)` when alias and name differ. Keep action-oriented titles for create pages and modal titles only (ADR-0130).
- **Place the Type B tab bar in the `FormWorkspace` `tabs` slot with `variant="band"` (ADR-0183), not in `children`.** The slot renders a full-bleed sub-header band whose divider spans the panel edge to edge, aligned with the header `border-b` and footer `border-t`. A tab bar left inside the centered `mx-auto max-w-*` content column draws a short baseline that shifts with viewport width / zoom and clashes with the full-bleed header and footer dividers. Tab _content_ stays in `children`, gated by `activeKey` (keep inactive tabs mounted via `hidden`, ADR-0164). Do not fake full-bleed with negative-margin hacks such as `-mx-5 lg:-mx-8`. For Type C builders without `FormWorkspace`, place `EditorTabs variant="band"` in a `sticky top-0 border-b` band at the top of the scroll column instead of inside the `mx-auto` wrapper.
- **Tab labels are text-only (ADR-0104).** Do not put icons (`IconXxx` / `<Icon…/>`) in `EditorTabs` `tabs[].label` or AntD `Tabs` `items[].label` for navigation/editing/section/detail/modal/inspector tabs — including the `권한 / Permissions` tab (use `t('common.permissions')`, never `IconLock` + text). Never mix: a single tab strip must not have icons on some tabs and text on others (Material Design consistency rule). The only exception is a **view-mode switcher** that toggles representations of the _same_ content (e.g. graph/table/text in Graph Explorer) — that is a segmented control, where icons are allowed. Count badges (`AppTag` item counts) are not icons and remain allowed.
- Type A create/register forms must not add tabs just to match Type B screens; keep the task visible in a single flow.
- Multiple cards can still be a Type A create flow when the work is sequential, such as prompt identity → template body → variable declarations → render preview.
- Use `FormWorkspace` for the page shell, `ResourceFormLayout` for primary/rail/full-width section placement, `SectionCard` for domain panels, and `ResourceBasicInfoSection` or compound `ResourceBasicInfo.*` for the first identity panel.
- Page-level editors should declare each section's semantic kind through `ResourceFormLayout.Section` instead of hand-rolling grid columns, sticky rails, or width classes.
- When viewport width allows, `ResourceFormLayout` places wide work sections such as identity, parameters, code, schema, and template in the primary column, and compact support sections such as configuration, reference, credentials, connectors, dependencies, and advanced options in the rail. Permission, table, and preview sections default to full width.
- Do not pair unrelated, table-heavy, editor-heavy, permission-heavy, preview-heavy, or severely height-mismatched panels just to fill horizontal space.
- Naming terminology such as 만들기/생성/등록/편집/수정 and create/register/edit/update is intentionally out of scope for ADR-0093 and should be handled by a UX writing decision.

## ResourceBasicInfoSection (`src/components/ResourceBasicInfoSection.tsx`)

Use `ResourceBasicInfoSection` or the compound `ResourceBasicInfo` API for the first "기본 정보 / Basic Info" panel in resource create/edit screens. The standard field set is:

- `name`
- `alias`
- `collection`
- `description`
- `tags`

All new resource create/edit forms should include this full set unless the backend API explicitly does not support the field. Reuse the shared API instead of hand-building parallel grids so helper text size, spacing, AI generate affordances, and tag input behavior stay consistent across Knowledge, Model, Agent, Connector, and future resource editors.

The section composes `SectionCard`, `FormSectionLayout`, `FormFieldGrid`, `CollectionSelect`, `TagInput`, and optional `InlineAIField` actions. It assumes the surrounding screen owns the Ant Design `Form` instance and maps submit payload fields such as `collection_id` and `tags`. `SectionCard` applies the shared AntD form label/helper rhythm to its content, so page-level editors should not add one-off label, required-marker, or helper-text classes around each card. Persisted editors pass `ResourceRecordInfoSection` through the `recordInfo` slot; the card then owns the responsive field + metadata composition instead of rendering record metadata as a separate sibling card.

When using `ResourceBasicInfoSection`, always wire the standard inline AI actions for the generated identity fields: pass `aliasAi`, `descriptionAi`, and `tagsAi`. These props are part of the component's standard create/edit contract, not optional polish. Each action should use the shared auto-generation hook for the current resource context, disable itself until `name` has a trimmed value, and provide localized `title` / `disabledTitle` strings. If a resource is intentionally read-only, built-in, or otherwise must not expose AI generation, make that explicit in the prop expression (for example by passing `undefined` from a named `isBuiltin` / `isReadOnly` guard) instead of simply omitting the props.

Full-page resource create/edit screens should place this section inside the shared resource form stack:

1. `FormWorkspace` for page header, scroll body, and bottom action bar.
2. `ResourceFormLayout` for semantic section placement.
3. `ResourceBasicInfoSection` or compound `ResourceBasicInfo.*` for the first identity panel.
4. `SectionCard` for domain-specific panels such as credentials, template body, deployment, preview, and permissions.

`ResourceFormLayout.Section` accepts a semantic `kind` and an optional `placement` override. Default placement keeps wide editing work in the primary column (`identity`, `parameters`, `code`, `schema`, `template`), compact support work in the rail (`configuration`, `reference`, `credentials`, `connectors`, `dependencies`, `advanced`), and broad review surfaces full width (`permissions`, `table`, `preview`). The rail uses a container query, so it appears only when the actual form container has enough width after app chrome and page padding. Use `placement` only for resource-specific exceptions; do not add page-level grid classes for normal create/edit forms.

Do not recreate the basic-information panel with raw `Form.Item` grids, standalone `CollectionSelect`, or standalone `TagInput` in page-level editors. That path quickly drifts in label naming, helper text size, input height, dark-mode borders, and action placement. Optional scope is communicated in helper text, while labels stay stable (`컬렉션` / `Collection`, not one-off variants such as `컬렉션 (선택)`).

`SectionCard` defaults to the quiet productive-form treatment: tokenized card surface, a compact text header, optional bare 18px leading icon, and no persistent colored rail. Use the explicit `variant="accent"` treatment only when a section needs a durable semantic signal; routine resource editors should stay on the default form variant so the input structure remains the visual focus. The content area carries the standard form selector classes for AntD `Form.Item` descendants:

- label: `text-sm font-semibold text-text-primary`, with compact label-to-control spacing
- helper / extra text: `text-xs leading-5 text-text-tertiary`
- required marker: status-failed token, separated from the label by a small gap
- AI affordance: field-level reveal on hover/focus/loading; no always-visible sparkle in the idle state

Section-level controls — a structured ↔ JSON view switch, a "show all" toggle, a small secondary action — go in the `headerActions` slot, which renders them at the trailing edge of the header row (mirroring AntD `Card` `extra`). Do not place a control that acts on the whole section in the content body: a right-aligned toggle on its own row reads as content, wastes a dedicated band, and inverts the control-over-target hierarchy. `headerActions` isolates its own click/key events from the collapse toggle, so it is safe on `collapsible` cards.

### Selection Rule

| Case                                                                   | Use                                  |
| ---------------------------------------------------------------------- | ------------------------------------ |
| The screen renders the standard 5-field identity block                 | `ResourceBasicInfoSection`           |
| The screen adds domain fields inside the same identity panel           | Compound `ResourceBasicInfo.*` parts |
| The screen omits standard fields because the API does not support them | Compound `ResourceBasicInfo.*` parts |
| The screen needs a different grid or a read-only metadata side panel   | Compound `ResourceBasicInfo.*` parts |

For screens that need the same visual rhythm but include extra domain fields, use the compound `ResourceBasicInfo` API instead of adding more mode-specific props or boolean flags to `ResourceBasicInfoSection`. Compose `ResourceBasicInfo.Root`, `Grid`, `Name`, `Alias`, `Collection`, `Description`, `Tags`, and generic `Field` slots so custom fields such as model `type`, `protocol`, `modelIdentifier`, or agent mode selection can sit inside the same basic-info panel without duplicating spacing and AI affordance logic.

## ResourceRecordInfoSection (`src/components/ResourceRecordInfoSection.tsx`)

Use `ResourceRecordInfoSection` for read-only metadata on persisted resource editors and settings surfaces. Render it inside the owning basic-information card through `ResourceBasicInfo.Root` / `ResourceBasicInfoSection`'s `recordInfo` slot, not as an independent vertical card. Wide containers place the flat metadata panel to the right of editable identity fields; narrow containers stack it below those fields while retaining one outer card. Preserve the canonical field order: copyable `ID`, `Owner`, `Created At`, then `Updated`. The header uses the fingerprint glyph to distinguish immutable record identity from editable resource fields. The component keeps unavailable legacy metadata visible as an em dash so the layout does not change by resource type. Pass `compact` only when the section is embedded in a narrow side panel. Builder surfaces that do not display the persisted name elsewhere may pass the optional `name` prop; normal editors should not repeat the name.

Do not duplicate a domain identifier that is already the primary immutable field of the editor. For example, an OIDC client's `client_id` remains in the basic-info form in both create and edit modes; it is the protocol identifier rather than separate portal record metadata.

## Notification Utilities (`src/utils/notifications.tsx`)

All user feedback notifications **must** use these utility functions. Do **not** use Ant Design `message.*()` or `App.useApp().message.*()` APIs directly. The utility is bound to Ant Design's `App.useApp().notification` at the app provider level so notifications inherit the active Ant Design context.

### showSuccessNotification

| Param         | Type     | Default  | Description        |
| ------------- | -------- | -------- | ------------------ |
| `message`     | `string` | required | Notification text  |
| `description` | `string` | —        | Optional subtitle  |
| `duration`    | `number` | `4`      | Auto-close seconds |

### showErrorNotification

| Param         | Type              | Default  | Description                          |
| ------------- | ----------------- | -------- | ------------------------------------ |
| `message`     | `string`          | required | Error title                          |
| `description` | `string`          | —        | Optional subtitle                    |
| `error`       | `Error \| string` | —        | Error detail with "View details" btn |
| `duration`    | `number`          | `5`      | Auto-close seconds                   |

### showInfoNotification / showWarningNotification

Same signature as `showSuccessNotification`.

### showDeleteNotification

Structured delete feedback with item name and type.

| Param      | Type              | Default  | Description             |
| ---------- | ----------------- | -------- | ----------------------- |
| `itemName` | `string`          | required | Deleted item name       |
| `itemType` | `string`          | required | Entity type (see below) |
| `success`  | `boolean`         | `true`   | Success or failure      |
| `error`    | `Error \| string` | —        | Error details           |

Supported `itemType` values: `dataset`, `code`, `pipeline`, `document`, `collection`, `knowledge`, `entity`, `relation`, `agent`, `tool`, `actor`, `workflow`, `dashboard`, `report`, `user`, `group`, `connector`.

### Usage

```typescript
import { showSuccessNotification, showErrorNotification } from '@utils/notifications';

// Success
showSuccessNotification({ message: t('item.created') });

// Error with details
showErrorNotification({
  message: t('item.createFailed'),
  error: err instanceof Error ? err : undefined,
});

// Delete
showDeleteNotification({ itemName: dataset.name, itemType: 'dataset' });
```

---

## UI Primitives (`src/components/ui/`)

All primitives use CVA (Class Variance Authority) for variant management and `cn()` for Tailwind class merging.

### Button

**Import**: `import { Button } from '@components/ui/button'`

| Prop      | Type                                                                          | Default     | Description                            |
| --------- | ----------------------------------------------------------------------------- | ----------- | -------------------------------------- |
| `variant` | `'default' \| 'destructive' \| 'outline' \| 'secondary' \| 'ghost' \| 'link'` | `'default'` | Visual style                           |
| `size`    | `'default' \| 'xs' \| 'sm' \| 'lg' \| 'icon'`                                 | `'default'` | Button size                            |
| `icon`    | `ReactNode`                                                                   | —           | Leading icon element                   |
| `loading` | `boolean`                                                                     | `false`     | Shows spinner, disables interaction    |
| `asChild` | `boolean`                                                                     | `false`     | Render as child element via Radix Slot |

```tsx
<Button variant="outline" size="sm" icon={<IconPlus size={16} />}>
  {t('common.create')}
</Button>
<Button variant="destructive" loading={isDeleting}>
  {t('common.delete')}
</Button>
```

### Input

**Import**: `import { Input } from '@components/ui/input'`

Wraps Ant Design `Input` with design-system theming. Single unified appearance across all surfaces — 모달·인스펙터·설정·페이지 어디서든 동일한 배경·보더·포커스 링을 사용한다.

| Prop        | Type                   | Default | Description         |
| ----------- | ---------------------- | ------- | ------------------- |
| `inputSize` | `'sm' \| 'md' \| 'lg'` | `'md'`  | Input sizing        |
| `error`     | `boolean`              | `false` | Error state styling |
| `leftIcon`  | `ReactNode`            | —       | Left icon slot      |
| `rightIcon` | `ReactNode`            | —       | Right icon slot     |

```tsx
<Input inputSize="md" error={!!errors.name} placeholder={t('common.enterName')} />
```

Tokens: `--input-bg`, `--input-border`, `--input-text`, `--input-placeholder`, `--input-focus-border`, `--input-focus-ring`, `--input-disabled-*`. `form-input` 클래스가 부여되어 `input.form-input:focus` 전역 규칙(`box-shadow: 0 0 0 2px var(--input-focus-ring)`)을 상속한다.

**높이 규약** — `Input`, `Textarea` (single-line), `Select`, `TagInput` 이 동일 size 에서 동일 픽셀 높이를 보장한다:

| Size | Height | Font size |
| ---- | ------ | --------- |
| `sm` | 32px   | xs        |
| `md` | 40px   | sm        |
| `lg` | 48px   | base      |

> **Deprecated**: `variant="modal"` 프롭은 제거되었다. 모달 내부 입력 필드도 default 그대로 사용.

### Textarea

**Import**: `import { Textarea } from '@components/ui/textarea'`

Wraps Ant Design `TextArea`. Shares the same tokens as `Input`.

| Prop           | Type                   | Default | Description         |
| -------------- | ---------------------- | ------- | ------------------- |
| `textareaSize` | `'sm' \| 'md' \| 'lg'` | `'md'`  | Textarea sizing     |
| `error`        | `boolean`              | `false` | Error state         |
| `resizable`    | `boolean`              | `false` | Allow manual resize |

> `variant="modal"` 제거. Textarea 역시 모달 안팎에서 동일 스타일.

### Select

**Import**: `import { Select } from '@components/ui/select'`

Wraps Ant Design `Select`. Exposes `.Option` and `.OptGroup` as sub-components. 동일한 `--input-*` 토큰을 `.ant-select-selector`에 적용한다.

| Prop         | Type                   | Default | Description   |
| ------------ | ---------------------- | ------- | ------------- |
| `selectSize` | `'sm' \| 'md' \| 'lg'` | `'md'`  | Select sizing |
| `error`      | `boolean`              | `false` | Error state   |

> `variant="modal"` 제거.

### Checkbox / CheckboxGroup

**Import**: `import { Checkbox, CheckboxGroup } from '@components/ui/checkbox'`

| Prop                | Type                                  | Default      | Description      |
| ------------------- | ------------------------------------- | ------------ | ---------------- |
| `variant`           | `'default' \| 'modal' \| 'inspector'` | `'default'`  | Context variant  |
| `checkboxSize`      | `'sm' \| 'md' \| 'lg'`                | `'md'`       | Checkbox sizing  |
| `direction` (group) | `'horizontal' \| 'vertical'`          | `'vertical'` | Layout direction |

### Radio / RadioGroup

**Import**: `import { Radio, RadioGroup } from '@components/ui/radio'`

Same variant/size system as Checkbox. Wraps Ant Design `Radio`.

### Switch

**Import**: `import { Switch } from '@components/ui/switch'`

| Prop         | Type                                  | Default     | Description     |
| ------------ | ------------------------------------- | ----------- | --------------- |
| `variant`    | `'default' \| 'modal' \| 'inspector'` | `'default'` | Context variant |
| `switchSize` | `'sm' \| 'md' \| 'lg'`                | `'md'`      | Switch sizing   |

### FormField

**Import**: `import { FormField } from '@components/ui/form-field'`

Compound form field with label, error, description, and action slots.

| Prop          | Type                   | Default     | Description                           |
| ------------- | ---------------------- | ----------- | ------------------------------------- |
| `label`       | `ReactNode`            | —           | Field label                           |
| `required`    | `boolean`              | `false`     | Shows required asterisk               |
| `error`       | `string`               | —           | Error message text                    |
| `description` | `string`               | —           | Help text below field                 |
| `variant`     | `'default' \| 'modal'` | `'default'` | Context variant                       |
| `actions`     | `ReactNode`            | —           | Action slot (e.g., "Generate" button) |

```tsx
<FormField label={t('common.name')} required error={errors.name}>
  <Input error={!!errors.name} />
</FormField>
```

> `FormField`/`FormLabel` 의 `variant="modal"` 은 라벨/설명 텍스트 톤을 모달 배경에 맞추기 위한 **잔존 옵션**이다. 입력 자식은 항상 default `Input`/`Textarea`/`Select` 를 사용한다.

### FormLabel

**Import**: `import { FormLabel } from '@components/ui/form-label'`

| Prop        | Type                   | Default     | Description        |
| ----------- | ---------------------- | ----------- | ------------------ |
| `variant`   | `'default' \| 'modal'` | `'default'` | Context variant    |
| `labelSize` | `'sm' \| 'md' \| 'lg'` | `'md'`      | Label sizing       |
| `required`  | `boolean`              | `false`     | Shows red asterisk |
| `disabled`  | `boolean`              | `false`     | Muted styling      |

### SearchInput

**Import**: `import { SearchInput } from '@components/ui/search-input'`

Wraps Ant Design `Input.Search` with search button.

| Prop        | Type                   | Default     | Description     |
| ----------- | ---------------------- | ----------- | --------------- |
| `variant`   | `'default' \| 'modal'` | `'default'` | Context variant |
| `inputSize` | `'sm' \| 'md' \| 'lg'` | `'md'`      | Sizing          |

### ListSearchInput

**Import**: `import { ListSearchInput } from '@components/ui/list-search-input'`

Standalone search input for list pages. Does NOT wrap Ant Design — uses native `<input>` with `IconSearch` prefix.

The outer container owns the background, border, and focus ring. The inner native input remains transparent, including on hover, so global input state styles cannot create a second background layer.

| Prop        | Type                   | Default | Description               |
| ----------- | ---------------------- | ------- | ------------------------- |
| `inputSize` | `'sm' \| 'md' \| 'lg'` | `'md'`  | Sizing                    |
| `iconSize`  | `number`               | —       | Custom icon size override |

### FilterInput

**Import**: `import { FilterInput } from '@components/ui/filter-input'`

Search/filter input with `IconSearch` prefix and `allowClear`. Wraps Ant Design `Input`.

| Prop        | Type                   | Default     | Description      |
| ----------- | ---------------------- | ----------- | ---------------- |
| `variant`   | `'default' \| 'modal'` | `'default'` | Context variant  |
| `inputSize` | `'sm' \| 'md' \| 'lg'` | `'md'`      | Sizing           |
| `showIcon`  | `boolean`              | `true`      | Show search icon |

### StatusBadge

**Import**: `import { StatusBadge } from '@components/ui/status-badge'`

Pipeline/batch execution status badge with colored dot.

| Prop      | Type                                                                                                    | Default     | Description                     |
| --------- | ------------------------------------------------------------------------------------------------------- | ----------- | ------------------------------- |
| `status`  | `'running' \| 'success' \| 'completed' \| 'failed' \| 'error' \| 'scheduled' \| 'pending' \| 'default'` | `'default'` | Status type                     |
| `size`    | `'sm' \| 'default' \| 'lg'`                                                                             | `'default'` | Badge size                      |
| `showDot` | `boolean`                                                                                               | `true`      | Show status dot                 |
| `pulse`   | `boolean`                                                                                               | `false`     | Pulse animation (for `running`) |

```tsx
<StatusBadge status="running" pulse />
<StatusBadge status="failed" size="sm" />
```

### StatsCard

**Import**: `import { StatsCard } from '@components/ui/stats-card'`

Dashboard statistics card with title, value, trend, and loading state.

| Prop          | Type                                               | Default     | Description     |
| ------------- | -------------------------------------------------- | ----------- | --------------- |
| `title`       | `string`                                           | required    | Card title      |
| `value`       | `string \| number`                                 | required    | Primary metric  |
| `description` | `string`                                           | —           | Subtitle text   |
| `icon`        | `ReactNode`                                        | —           | Leading icon    |
| `trend`       | `{ value, label?, isPositive? }`                   | —           | Trend indicator |
| `variant`     | `'default' \| 'elevated' \| 'outline' \| 'filled'` | `'default'` | Card variant    |
| `size`        | `'sm' \| 'default' \| 'lg'`                        | `'default'` | Card size       |
| `loading`     | `boolean`                                          | `false`     | Show skeleton   |

### KeyboardShortcut

**Import**: `import { KeyboardShortcut } from '@components/ui/keyboard-shortcut'`

Renders keyboard shortcut keys as styled `<kbd>` elements.

| Prop        | Type       | Default  | Description           |
| ----------- | ---------- | -------- | --------------------- |
| `keys`      | `string[]` | required | Array of key labels   |
| `separator` | `string`   | `'+'`    | Key separator display |

```tsx
<KeyboardShortcut keys={['⌘', 'K']} />
```

### Spinner

**Import**: `import { Spinner } from '@components/ui/spinner'`

Animated loading spinner using `IconLoader2`.

| Prop       | Type                   | Default | Description                |
| ---------- | ---------------------- | ------- | -------------------------- |
| `size`     | `'sm' \| 'md' \| 'lg'` | `'md'`  | 20px / 24px / 32px         |
| `fullPage` | `boolean`              | `false` | Center in viewport         |
| `text`     | `string`               | —       | Loading text below spinner |

### InlineSpinner

**Import**: `import { InlineSpinner } from '@components/ui/inline-spinner'`

Minimal inline spinner that inherits `currentColor`. For button/text contexts.

| Prop   | Type                   | Default | Description        |
| ------ | ---------------------- | ------- | ------------------ |
| `size` | `'sm' \| 'md' \| 'lg'` | `'md'`  | 14px / 16px / 20px |

### LoadingOverlay

**Import**: `import { LoadingOverlay } from '@components/ui/loading-overlay'`

Semi-transparent overlay with centered spinner. Positioned absolute — requires relative parent.

| Prop   | Type     | Default | Description         |
| ------ | -------- | ------- | ------------------- |
| `text` | `string` | —       | Loading text        |
| `size` | `number` | —       | Custom spinner size |

---

## Shared Components (`src/components/`)

### ActionCard

Multi-variant card for displaying domain entities.

| Prop          | Type                                       | Description                       |
| ------------- | ------------------------------------------ | --------------------------------- |
| `title`       | `string`                                   | Card title                        |
| `description` | `string`                                   | Optional subtitle                 |
| `selected`    | `boolean`                                  | Selected state                    |
| `variant`     | `'default' \| 'stats' \| 'item' \| 'list'` | Card style variant                |
| `category`    | `EntityType`                               | Entity category for icon coloring |
| `onClick`     | `() => void`                               | Click handler                     |

### AnnouncementBanner

System-wide announcement banner. Data-driven via React Query — no props needed.

### AppInfoModal

Application info modal (version, git commit, build time).

| Prop      | Type         | Description      |
| --------- | ------------ | ---------------- |
| `open`    | `boolean`    | Modal visibility |
| `onClose` | `() => void` | Close handler    |

### AppTag

Base pill tag component wrapping Ant Design `Tag`. Shared pill used for collection meta tags, search result tags, and anywhere a neutral-to-primary-tinted label is needed.

| Prop       | Type                                | Default     | Description              |
| ---------- | ----------------------------------- | ----------- | ------------------------ |
| `variant`  | `'default' \| 'label' \| 'neutral'` | `'default'` | Tag style                |
| `truncate` | `boolean`                           | `false`     | Truncate long text       |
| `maxWidth` | `string`                            | —           | Max width for truncation |

**Variant styles** (see [PATTERNS.md §1.5 Tinted surface pattern](./PATTERNS.md#15-tinted-surface-pattern-container--on-container)):

| Variant   | Background                              | Text                                        | Use for                                                   |
| --------- | --------------------------------------- | ------------------------------------------- | --------------------------------------------------------- |
| `default` | Ant Design default                      | Ant Design default                          | Generic tags                                              |
| `label`   | `bg-slate-200` (L) / `bg-slate-700` (D) | `text-slate-700` (L) / `text-slate-200` (D) | Meta/label tags on detail pages (user-generated metadata) |
| `neutral` | `bg-surface-muted`                      | `text-text-secondary`                       | Secondary / "not yet classified" tags                     |

> The `label` variant uses a neutral slate pair (≈ 9.45:1 contrast, AAA in both themes) so that passive metadata pills do not compete with brand-colored surfaces (active tabs, focused inputs, selected rows). Reserve `primary-container` strictly for surfaces that genuinely imply a brand relationship per [PATTERNS.md §1.5](./PATTERNS.md#15-tinted-surface-pattern-container--on-container).

### CardSkeletonLoader

Grid of skeleton loading placeholders for card list pages.

| Prop    | Type     | Default | Description              |
| ------- | -------- | ------- | ------------------------ |
| `count` | `number` | `6`     | Number of skeleton cards |

### TableSkeleton

Table-shaped skeleton for list-page cold loads. Replaces Ant Design `Table`'s built-in `loading` Spin so the page reveals structure (header + rows) while data arrives. See PATTERNS.md §10.

| Prop         | Type      | Default | Description                                   |
| ------------ | --------- | ------- | --------------------------------------------- |
| `rows`       | `number`  | `6`     | Skeleton row count                            |
| `columns`    | `number`  | `4`     | Visible column count (exclude actions column) |
| `showHeader` | `boolean` | `true`  | Render a header strip                         |
| `className`  | `string`  | —       | Extra classes for the container               |

```tsx
{isLoading && items.length === 0 ? (
  <TableSkeleton rows={8} columns={5} />
) : (
  <Table dataSource={items} columns={columns} ... />
)}
```

### ListLoadError

Shared error state for list pages whose React Query fetch fails. Wraps `classifyListLoadError` (a pure helper that buckets the error into `network` / `forbidden` / `server` / `generic`) with `ListEmptyState`'s `error` variant so every list page surfaces failure with the same icon, tone, and copy. See ADR-0057 and PATTERNS.md §5.

| Prop        | Type                                                | Default | Description                                                                                       |
| ----------- | --------------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------- |
| `error`     | `unknown`                                           | —       | The React Query / `ApiError` error object.                                                        |
| `resource`  | `string`                                            | —       | Localized resource name used in `{{resource}}` interpolation (e.g. `t('xxx.list.resourceName')`). |
| `onRetry`   | `() => void`                                        | —       | Retry handler. Usually `() => refetch()`.                                                         |
| `forceKind` | `'network' \| 'forbidden' \| 'server' \| 'generic'` | —       | Override the classifier — used by multi-query pages (Recents) to surface a single `generic` copy. |

Place the error branch **before** the loading/empty branches so the user sees the failure surface as soon as the query resolves:

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

i18n keys live under `common.listErrorState.{network,forbidden,server,generic}.{title,description}` — each page only contributes its own `xxx.list.resourceName` for the `{{resource}}` interpolation.

### ListToolbar

Shared shell for the search/filter strip directly below a list page header. It owns the standard page padding, background, sticky offset option, and responsive row/column behavior. Compose it with `ListSearchInput`, `ListSegmentedFilter`, and existing dropdown filters instead of hand-rolling `px-6 lg:px-8 pt-5 pb-5` wrappers in every page.

| Prop               | Type        | Default | Description                                      |
| ------------------ | ----------- | ------- | ------------------------------------------------ |
| `sticky`           | `boolean`   | `false` | Use the standard `top-[88px]` sticky list offset |
| `className`        | `string`    | —       | Extra classes for the outer toolbar shell        |
| `contentClassName` | `string`    | —       | Extra classes for the inner flex row             |
| `children`         | `ReactNode` | —       | Toolbar controls                                 |

### ListSegmentedFilter

Compact segmented control for list-page filters such as status, type, provider, and scope. Options accept an optional count and leading indicator. Use this for list filters; reserve `SegmentedControl` for form/editor radio groups.

| Prop              | Type                          | Default | Description                     |
| ----------------- | ----------------------------- | ------- | ------------------------------- |
| `options`         | `ListSegmentedFilterOption[]` | —       | Filter choices                  |
| `value`           | `string`                      | —       | Active choice                   |
| `onChange`        | `(value) => void`             | —       | Selection handler               |
| `ariaLabel`       | `string`                      | —       | Accessible label for the group  |
| `className`       | `string`                      | —       | Extra classes for the container |
| `optionClassName` | `string`                      | —       | Extra classes for each option   |

### CategoryIcon

Resolves a category string to its icon with optional colored background.

| Prop             | Type      | Description                                          |
| ---------------- | --------- | ---------------------------------------------------- |
| `category`       | `string`  | Category identifier (dataset subtype, code language) |
| `size`           | `number`  | Icon size                                            |
| `withBackground` | `boolean` | Show colored background circle                       |

### CategoryTag

Category pill tag with icon.

| Prop       | Type           | Description   |
| ---------- | -------------- | ------------- |
| `category` | `CategoryType` | Category type |
| `size`     | `'sm' \| 'md'` | Tag size      |

### CollectionItemDeleteModal

Single-action permanent-delete modal for collection items. Requires typing the item name to
confirm. (ADR-0069 removed the former "remove from collection" detach option — every resource
must belong to a collection, so detach is no longer a supported action.)

| Prop                | Type         | Description              |
| ------------------- | ------------ | ------------------------ |
| `isOpen`            | `boolean`    | Modal visibility         |
| `onPermanentDelete` | `() => void` | Permanent delete handler |
| `itemName`          | `string`     | Item display name        |
| `itemType`          | `string`     | Entity type              |

### CollectionSelect

Collection dropdown with optional inline creation of new collections. Dropdown options render
as two-line rows with a leading collection icon, matching the global search row structure:
collection alias as the primary line and the raw collection name as the secondary line. Search
matches alias, name, and id.

When inline creation is enabled, the dropdown footer renders a same-level "create collection"
row. Selecting it closes the dropdown and opens a modal for the collection name and optional
alias.

| Prop          | Type             | Default | Description                       |
| ------------- | ---------------- | ------- | --------------------------------- |
| `valueField`  | `'id' \| 'name'` | `'id'`  | Which field to use as value       |
| `allowCreate` | `boolean`        | `false` | Enable inline collection creation |

### CollectionPickerDropdown

Controlled searchable collection picker used by the global navbar collection scope and reusable wherever a lightweight collection chooser is needed. It shares the same searchable option rendering as the global scope selector and can clear invalid selected values after the authorized collection list loads.

| Prop               | Type                           | Default                 | Description                                 |
| ------------------ | ------------------------------ | ----------------------- | ------------------------------------------- |
| `value`            | `string \| null`               | —                       | Selected collection id, or `null` for all   |
| `onChange`         | `(id: string \| null) => void` | —                       | Selection handler                           |
| `allLabel`         | `string`                       | navbar i18n all label   | Label for the all-collections option        |
| `allSubtitle`      | `string`                       | navbar i18n scope label | Subtitle for the all-collections option     |
| `tooltipTitle`     | `ReactNode`                    | —                       | Optional trigger tooltip                    |
| `placement`        | `PopoverProps['placement']`    | `'bottomLeft'`          | Popover placement                           |
| `triggerClassName` | `string`                       | —                       | Width/margin overrides for the trigger      |
| `panelWidth`       | `number`                       | `292`                   | Dropdown panel width                        |
| `onInvalidValue`   | `() => void`                   | —                       | Called when `value` is no longer authorized |

### CollectionTag

Pill tag for collection names with the shared collection entity icon. In table-based resource lists, render collection metadata immediately after the name column unless the page has a documented ordering exception (see PATTERNS.md §5.6).

| Prop   | Type     | Description     |
| ------ | -------- | --------------- |
| `name` | `string` | Collection name |

### CollectionTreePanel

Full-featured collection tree sidebar with search, drag-and-drop, expand/collapse.

| Key Prop               | Type             | Description                      |
| ---------------------- | ---------------- | -------------------------------- |
| `treeNodes`            | `TreeDataNode[]` | Tree data                        |
| `selectedNodeId`       | `string`         | Currently selected node          |
| `onNodeClick`          | `(key) => void`  | Node click handler               |
| `draggableEntityTypes` | `string[]`       | Entity types that can be dragged |
| `collapsible`          | `boolean`        | Enable panel collapse            |

### CsvDropzone

Drag-and-drop CSV file upload zone with status display.

| Prop              | Type             | Description            |
| ----------------- | ---------------- | ---------------------- |
| `csvUploadStatus` | `UploadStatus`   | Current upload state   |
| `onFileSelect`    | `(file) => void` | File selection handler |

### DatasetSelect

Dataset dropdown, optionally filtered by collection name.

| Prop             | Type     | Description          |
| ---------------- | -------- | -------------------- |
| `collectionName` | `string` | Filter by collection |

### DeleteDatasetWithReferencesModal

Dataset deletion modal that checks for pipeline references.

| Prop        | Type         | Description                     |
| ----------- | ------------ | ------------------------------- |
| `isOpen`    | `boolean`    | Modal visibility                |
| `datasetId` | `string`     | Dataset to check references for |
| `onConfirm` | `() => void` | Confirm delete handler          |

### DeleteModal

Generic delete confirmation with name-typing verification.

| Prop        | Type         | Description                    |
| ----------- | ------------ | ------------------------------ |
| `isOpen`    | `boolean`    | Modal visibility               |
| `itemName`  | `string`     | Name user must type to confirm |
| `itemType`  | `string`     | Entity type label              |
| `onConfirm` | `() => void` | Confirm handler                |
| `isLoading` | `boolean`    | Loading state                  |

### DetailHeader

Breadcrumb-style header showing collection > item path with category tag.

| Prop             | Type     | Description       |
| ---------------- | -------- | ----------------- |
| `collectionName` | `string` | Parent collection |
| `itemName`       | `string` | Current item      |
| `category`       | `string` | Entity category   |

### DuplicateModal

Resource duplication dialog with name, alias, description, tags, and collection selection.

| Prop           | Type             | Description          |
| -------------- | ---------------- | -------------------- |
| `isOpen`       | `boolean`        | Modal visibility     |
| `originalName` | `string`         | Source resource name |
| `itemType`     | `string`         | Entity type          |
| `onDuplicate`  | `(data) => void` | Duplication handler  |

### EntityIcon

Unified entity icon with consistent color theming per entity type.

| Prop             | Type         | Description                  |
| ---------------- | ------------ | ---------------------------- |
| `entityType`     | `EntityType` | Entity type for color lookup |
| `size`           | `number`     | Icon size                    |
| `withBackground` | `boolean`    | Show colored background      |

### SubjectIdentity

Canonical avatar + display-name row for any user, group, or public subject. Use this whenever a table cell, list row, dropdown option, detail header, or permission surface displays user-like identity information.

**Import**: `import { SubjectIdentity } from '@components'`

| Prop           | Type                | Default  | Description                                       |
| -------------- | ------------------- | -------- | ------------------------------------------------- |
| `subject`      | `PermissionSubject` | required | `{ subject_type, subject_id }` identity source    |
| `name`         | `string`            | required | Display name resolved by the caller               |
| `subtitle`     | `string`            | -        | Optional email or secondary label                 |
| `size`         | `number`            | `28`     | Avatar diameter in pixels                         |
| `avatarConfig` | `AvatarFullConfig`  | -        | Optional saved avatar config for user subjects    |
| `avatarUrl`    | `string`            | -        | Optional saved avatar image URL for user subjects |

```tsx
// Table column render
{
  title: t('common.labels.owner'),
  key: 'owner',
  render: (_, record) => (
    <SubjectIdentity
      subject={{ subject_type: 'user', subject_id: record.ownerId }}
      name={record.ownerName}
      subtitle={record.ownerEmail}
      size={24}
    />
  ),
}
```

Rules:

- Use `SubjectIdentity` for concrete users, groups, and public subjects when the subject belongs to row data or API data.
- Use `SubjectAvatar` directly only when layout needs custom text composition, as in collection detail owner chips.
- Use `OwnerCell` (`@components/permissions`) for per-row resource owner columns (including Recents) — it composes `SubjectIdentity` with a hover preview popover.
- Do not hand-roll avatar initials, colored circles, or user-name rows in feature pages.

### OwnerCell

Table-cell renderer for a resource's owner column. Shows the primary owner (avatar + display name), a `+N` badge when additional owners exist, and an `OwnerPreview` popover on hover.

**Import**: `import { OwnerCell } from '@components/permissions'`

| Prop           | Type                  | Default  | Description                                                                                             |
| -------------- | --------------------- | -------- | ------------------------------------------------------------------------------------------------------- |
| `owners`       | `PermissionSubject[]` | required | Owner subjects derived from permission grants (role=`owner`, non-public).                               |
| `fallbackName` | `string`              | -        | Legacy owner name from the resource itself. Treated as a synthetic user subject when `owners` is empty. |
| `emptyLabel`   | `string`              | `'—'`    | Shown when both `owners` and `fallbackName` are empty.                                                  |
| `size`         | `number`              | `20`     | Avatar diameter in pixels. Consistent list-page density across every owner column.                      |

```tsx
// List-page owner column
{
  title: t('common.labels.owner'),
  key: 'owner',
  render: (_, record) => (
    <OwnerCell
      owners={metadataById[record.id]?.owners ?? []}
      fallbackName={record.owner?.trim() || undefined}
    />
  ),
}
```

Pair with `useResourceAccessMetadata(items, resourceType)` from the same barrel to fetch `owners` for every row in one `useQueries` batch. See [`PATTERNS.md#identity-hover-preview`](./PATTERNS.md#identity-hover-preview) for the full pattern.

### OwnerPreview

Hover popover that shows the primary owner plus a peek at additional owners. Used internally by `OwnerCell`; expose directly when the trigger element is custom (e.g. an owner chip on a detail header).

**Import**: `import { OwnerPreview } from '@components/permissions'`

| Prop        | Type                  | Default                        | Description                                 |
| ----------- | --------------------- | ------------------------------ | ------------------------------------------- |
| `owners`    | `PermissionSubject[]` | required                       | Owners to preview (primary + up to 3 more). |
| `children`  | `ReactElement`        | required                       | Trigger element the popover wraps.          |
| `roleLabel` | `string`              | `t('permissions.roles.owner')` | Badge label shown in the preview header.    |

### useResourceAccessMetadata

Hook that fetches permission grants for a list of resources and returns owner + shared-status metadata keyed by resource id. Uses `useQueries` for parallel per-row lookups.

**Import**: `import { useResourceAccessMetadata } from '@components/permissions'`

```tsx
const { metadataById, isLoading } = useResourceAccessMetadata(dashboards, 'dashboard');
// metadataById[id] => { owners: PermissionSubject[], isSharedByPermission: boolean }
```

Also exports `buildResourceAccessMetadata(grants)` for callers that already have grants in hand.

### EntityTag

Pill tag for entity names with cube icon.

| Prop   | Type     | Description |
| ------ | -------- | ----------- |
| `name` | `string` | Entity name |

### FormSectionHeading

Semantic heading for form sections.

| Prop       | Type        | Default  | Description        |
| ---------- | ----------- | -------- | ------------------ |
| `children` | `ReactNode` | required | Heading text       |
| `level`    | `1-6`       | `3`      | HTML heading level |

### FlatCard

Shared shell for flat (no-preview) list cards with a left accent bar. Provides consistent border, background, hover lift, accent line, and accessible `<button>` wrapper. Used by Connector list page.

| Key Prop    | Type         | Description                                 |
| ----------- | ------------ | ------------------------------------------- |
| `accent`    | `string`     | CSS color for the left accent bar           |
| `menuItems` | `MenuItem[]` | 3-dot dropdown items (absolute top-right)   |
| `onClick`   | `() => void` | Card click handler                          |
| `children`  | `ReactNode`  | Card content (layout is consumer's concern) |
| `ariaLabel` | `string`     | Accessible label for the card button        |
| `className` | `string`     | Additional CSS classes                      |

CSS classes: `.flat-card` (accent bar, hover lift), `.flat-card-grid` (stagger entrance), `.flat-card-actions` (hover-reveal actions).

### HelpTooltip

Field-level help affordance: a `(?)` icon placed next to a label whose description appears on hover/focus. Use it instead of a permanent helper paragraph when the text is long enough to stretch a compact panel — but keep short, value-dependent hints (e.g. "선택한 모델 컨텍스트 윈도우: 200,000 토큰.") visible.

Import via the subpath `@components/HelpTooltip`. The trigger is a real `<button>` carrying the text as its `aria-label`: a bare SVG takes no focus, so a keyboard user could never open the tooltip. Never hand-roll `<Tooltip><IconHelpCircle /></Tooltip>` — it reintroduces that gap.

| Prop        | Type     | Description                                                      |
| ----------- | -------- | ---------------------------------------------------------------- |
| `title`     | `string` | Help text — shown on hover/focus and used as the accessible name |
| `className` | `string` | Additional CSS classes on the trigger                            |

### InlineAIField

Wraps a form control (`Input` / `Textarea` / `TagInput`) with an inline AI generate affordance — a quiet sparkle button anchored inside the field, plus a brand-rainbow conic-gradient sweep that runs around the field perimeter while AI generation is in flight. Implements the engagement-reveal pattern from ADR-0065. Pair with `AIGenerateButton` for the trigger; its default variant is the functional sparkle.

| Prop        | Type                                    | Default        | Description                                                                                                                                                                                                                     |
| ----------- | --------------------------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `children`  | `ReactNode`                             | required       | The form control to wrap (single child).                                                                                                                                                                                        |
| `button`    | `ReactNode`                             | required       | Typically `<AIGenerateButton … />`.                                                                                                                                                                                             |
| `anchor`    | `'inputRight' \| 'textareaBottomRight'` | `'inputRight'` | Trigger position: vertically centered right (single-line inputs / TagInput), or bottom-right (multi-line textarea, where the right edge holds the scrollbar/resize handle).                                                     |
| `isLoading` | `boolean`                               | `false`        | Renders the sweep border + ack pulse on the trigger. Pair with the same `isLoading` on the inner `AIGenerateButton` so the icon also stays engaged.                                                                             |
| `hasError`  | `boolean`                               | `false`        | Sets `data-ai-error="true"` so the sweep ring recolors to the danger status hue. After a loading→idle transition with `hasError=true`, the danger ring lingers for ~1s before fading. Caller toggles back to `false` to clear.  |
| `onCancel`  | `() => void`                            | —              | Cancel handler. When set AND `isLoading=true`, pressing `Escape` inside the wrapper aborts the in-flight call. Pair with `AIGenerateButton.onCancel` (same handler) so the morph-to-stop button also triggers cancel. ADR-0066. |
| `className` | `string`                                | —              | Optional wrapper class.                                                                                                                                                                                                         |

**Anchor → input padding** (the trigger reserves ~36px on its side; pad the inner control so text doesn't slide under it):

| Anchor                | Control    | Padding to apply                                        |
| --------------------- | ---------- | ------------------------------------------------------- |
| `inputRight`          | `Input`    | `className="!pr-9"`                                     |
| `inputRight`          | `TagInput` | `className="pr-9"`                                      |
| `textareaBottomRight` | `Textarea` | no padding change (anchor sits in scroll/resize corner) |

**Behavior**:

- Idle: the trigger is visually hidden until the user hovers or focuses the field, so AI reads as an auxiliary action rather than the form's steady visual identity.
- Hover / focus-visible on the trigger: show a tertiary `IconSparklesFilled` inside a 28px icon button, then cross-fade to the brand-rainbow `AISparkleIcon` (300 ms) on direct trigger hover/focus with a quiet surface hover and no added shadow.
- `isLoading=true`: 600 ms brightness pulse on the trigger + brand-rainbow conic sweep ring fades in around the field perimeter (4 s rotation). The browser/AntD focus ring on the form control is suppressed during loading so it doesn't stack with the sweep; the trigger button keeps its own focus-visible ring. `aria-busy="true"` is set on the wrapper for assistive tech, and a polite `role="status"` live region announces the transition ("AI is generating" → "AI generation complete" or "AI generation failed").
- `hasError=true`: the sweep ring recolors to `--color-status-failed`. When paired with `isLoading=false` (typical: caller sets `hasError` right as loading ends), the danger ring holds for ~1s before fading so the user sees _why_ the sweep ended. Caller is responsible for surfacing the error message (e.g. toast) and for clearing `hasError` back to `false` before the next attempt.
- **AI-touched residual** (automatic): on `isLoading: true → false` _without_ `hasError`, the wrapper sets `data-ai-just-touched="true"` for 2 seconds. CSS paints a fading background tint (`--color-ai-touched-fade`) on the form control during the window, then clears. Gives the user a peripheral cue that "AI just wrote this" — see ADR-0066.
- **Cancel during loading** (opt-in via `onCancel`): while `isLoading=true`, the sparkle trigger morphs into a `IconPlayerStopFilled` icon in the same position, tooltip flips to "Cancel AI generation", and clicking fires `onCancel` instead of `onClick`. ESC keypress inside the wrapper does the same. Caller wires `onCancel` on both the wrapper and the inner `AIGenerateButton` with the same handler (typically `() => cancel('description')` from `useAutoGenerateDescription`). When `onCancel` is unset, behavior is unchanged — the button stays disabled during loading (legacy). ADR-0066.
- Disabled trigger: hover/focus does not reveal the rainbow or the sweep (sweep is loading-only anyway).

**Conditional usage** (callers where the AI callback is optional, e.g. `onAutoGenerateXxx?: () => void`): wrap with an IIFE so the field renders raw when no callback is provided.

```tsx
const input = <Input … className={onGen ? '!pr-9' : undefined} />;
return onGen ? (
  <InlineAIField anchor="inputRight" isLoading={loading} button={<AIGenerateButton onClick={onGen} … />}>
    {input}
  </InlineAIField>
) : input;
```

**Out of scope**:

- Streaming reveal of generated content — backend dependency.

### AISkeleton

Loading placeholder for **AI-generation content** — shimmer lines carrying the brand rainbow (same palette as `AISparkleIcon` / `.ai-sweep-border`). Use it where the app is waiting on model output that will fill a content area (e.g. the TTL condense draft preview), so the "AI is generating" wait is visually distinct from a plain data load, which keeps the quiet AntD `Skeleton`. Purely decorative (hidden from the a11y tree); motion respects `prefers-reduced-motion`.

| Prop        | Type     | Default | Notes                         |
| ----------- | -------- | ------- | ----------------------------- |
| `lines`     | `number` | `4`     | Number of shimmer lines.      |
| `className` | `string` | —       | Wrapper class (e.g. spacing). |

```tsx
import { AISkeleton } from '@components/ai';

{
  isGenerating ? <AISkeleton lines={6} /> : <ResultPreview data={result} />;
}
```

Do not replace purpose-built AI-wait affordances (`InlineAIField` sweep border, `AILoadingBorder`, `ThinkingIndicator`, `GenerationProgress`) with it — see the decision table in [PATTERNS.md §10](PATTERNS.md).

### ReportOutputPreview

Shared **report page gallery** for a completed report run: format toggle (only formats present in `outputs`), a large page view, page navigation, a thumbnail rail, and per-query retry on preview failures. Used by the chat `report-run` result card, the report run detail page, and the generation screen — one implementation, never a second gallery (ADR-0222 §2.2). Import from `@components/reports/ReportOutputPreview`.

| Prop     | Type                 | Notes                                                                                                                                                                                        |
| -------- | -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `run`    | `ReportRunManifest`  | Renders nothing unless `status === 'completed'` and at least one requested format exists in `outputs`.                                                                                       |
| `layout` | `'card'` \| `'page'` | `card` (default) keeps the chat card frame and dimensions. `page` drops the card chrome — the host page is already a panel — enlarges the thumbnails, and gives page blobs a real GC window. |

`ReportOutputPreviewLoading` is the in-generation placeholder (the weave animation) for chat cards. Copy lives under `reports.preview.*`.

### ReportDownloadButton

Shared **report output download CTA** for the run detail header, the generation progress screen, and the chat `report-run` card. A single available format renders a direct button labeled with that format (`PPTX 다운로드`) instead of a one-item dropdown that would repeat the download keyword; two or more formats render the `다운로드` dropdown with per-format items. Import from `@components/reports/ReportDownloadButton`.

| Prop         | Type                                                   | Notes                                                                                                              |
| ------------ | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------ |
| `items`      | `{ format: ReportOutputFormat; disabled?: boolean }[]` | Empty array renders a disabled `다운로드` button; per-item `disabled` keeps requested-but-missing formats visible. |
| `onDownload` | `(format: ReportOutputFormat) => void`                 | Called with the chosen format.                                                                                     |
| `size`       | `'sm'?`                                                | Chat-card sizing; omit for default.                                                                                |

### ChatComposer

Shared **chat / message composer** used by the agent chat, the knowledge chat, and the Runs AI analysis panel so every chat surface shares one visual contract: a seamless page background with a single rounded input box, an autosize textarea, inline controls, and a send/stop button. Reuse it for any follow-up / message input instead of hand-rolling a `Textarea` + send `<button>`. Import from `@components/chat/ChatComposer`.

| Prop                                               | Type                     | Notes                                                                                                                                                                                  |
| -------------------------------------------------- | ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `value` / `onChange`                               | `string` / `(v) => void` | Controlled input.                                                                                                                                                                      |
| `onSend` / `onStop`                                | `() => void`             | Send (button at rest) / stop (button while `isStreaming`).                                                                                                                             |
| `onKeyDown`                                        | `KeyboardEventHandler`   | Wire Enter-to-send here (Enter without Shift → `onSend`).                                                                                                                              |
| `isStreaming`                                      | `boolean`                | Swaps the send button for a stop button.                                                                                                                                               |
| `disabled` / `sendDisabled`                        | `boolean`                | Disable the textarea / override the default `!value.trim() \|\| disabled` send-disable.                                                                                                |
| `placeholder` / `hint` / `sendLabel` / `stopLabel` | `string`                 | Localized copy (`hint` is the small centered line under the box).                                                                                                                      |
| `maxRows`                                          | `number`                 | Autosize cap (default 4).                                                                                                                                                              |
| `topBadge` / `headerToolbar` / `toolbar`           | `ReactNode`              | Above the box / borderless control row above / inline controls left of send (`ModelChip`, reasoning chip).                                                                             |
| `edgeSlot`                                         | `ReactNode`              | Absolutely-positioned child of the box, for decoration hanging off its border (the ADR-0197 companion perch anchor). Out of flex flow, so unlike `leading`/`toolbar` it adds no `gap`. |

The agent chat wraps it as `AgentChatComposer` (injects the transcode badge as `topBadge`).

```tsx
import { ChatComposer } from '@components/chat/ChatComposer';

<ChatComposer
  value={input}
  onChange={setInput}
  onKeyDown={(e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      onSend();
    }
  }}
  onSend={onSend}
  onStop={stop}
  isStreaming={isRunning}
  placeholder={t('…')}
  hint={t('…')}
  sendLabel={t('…')}
  stopLabel={t('…')}
  toolbar={<ModelChip />}
/>;
```

### MessageTimestamp

Time stamp for a chat message bubble, shown **without hover** on every chat surface (agent chat,
assistant, Runs AI analysis, knowledge chat). Today renders the time alone (`오후 11:38` / `11:38
PM`); an older message carries its date (`8월 9일 오후 11:38` / `Aug 9, 11:38 PM`, plus the year
once it is from another one), because a bare time on a restored conversation reads as today. Format
follows the active i18n language via `Intl.DateTimeFormat`, and the `title` tooltip carries the full
date. Renders `null` when the time is unknown, so a restored message with no server stamp shows
nothing rather than a fabricated time (ADR-0204). Put it outside the hover-revealed action row —
hover-gate the copy/feedback buttons individually instead. Import from
`@components/chat/MessageTimestamp`.

> Partial `react-i18next` mocks in consumer tests must include `i18n: { language: 'en' }` alongside
> `t` — this component reads the active language.

| Prop        | Type             | Notes                                                        |
| ----------- | ---------------- | ------------------------------------------------------------ |
| `value`     | `TimestampInput` | Epoch ms/seconds, ISO string, `Date`, or nullish (→ `null`). |
| `className` | `string`         | Layout only (e.g. `ml-auto`); typography is fixed.           |

```tsx
import { MessageTimestamp } from '@components/chat/MessageTimestamp';

<span className="flex items-center gap-1 px-1">
  <CopyMessageButton content={text} className="opacity-0 group-hover/user:opacity-100" />
  <MessageTimestamp value={message.createdAt} />
</span>;
```

### WorkSummaryRow

Shared **turn work summary row** for chat surfaces — the folded one-liner above a completed answer
(`23m 39s 동안 작업 · 도구 5개 실행`). Owns only the fold shell: tinted container, chevron,
`aria-expanded` toggle, expanded body slot. The label, icon, and trailing segments are the caller's,
so the primitive carries no i18n or domain state. Import from `@components/chat/WorkSummaryRow`.
See ADR-0233.

- `label` — computed by the caller (running tool, or `worked` phrase with `formatElapsed(ms)`).
- `icon` / `trailing` — status glyph and secondary segments (tool count, failure count).
- `children` — the detail revealed on expand. **Omitted → renders as plain text, not a button**, so
  a row with nothing to reveal never presents a dead affordance.

Folded by default; it is not a live region (live ticking belongs to `GenerationStatus`). Elapsed
values come from `formatElapsed` / `formatElapsedPrecise` in `@utils/date` — the single duration
ladder (`0.8s` → `39s` → `23m 39s` → `1h 12m`) shared by the live timer and every settled turn.
Never re-derive a duration format inline.

```tsx
import { WorkSummaryRow } from '@components/chat/WorkSummaryRow';

<WorkSummaryRow
  label={t('agents.chat.toolTimeline.worked', { time: formatElapsed(elapsedMs) })}
  icon={<IconCheck className="size-3 text-status-success" aria-hidden="true" />}
  trailing={<span>· {t('agents.chat.toolTimeline.completed', { count })}</span>}
>
  {steps.map((step) => (
    <StepRow key={step.id} step={step} />
  ))}
</WorkSummaryRow>;
```

### LocaleSwitcher

Locale dropdown toggle (Korean/English). No props — uses SettingsContext.

### ModalActions

Shared modal/form **footer action row**. Encapsulates the cancel/confirm button contract that was
previously hand-rolled per dialog (order, gap, variant, size, loading, destructive, left auxiliary
slot). The **pair of `ModalTitle`/`WorkspaceHeader`** — same "one contract, both surfaces" rule for
the footer. See ADR-0095 and the [Modal Footer Actions pattern](PATTERNS.md).

- **AntD `<Modal>`**: `footer={<ModalActions … />}` — footer chrome comes from
  `getStandardModalStyles().footer`, so omit `surface`.
- **Custom portal modal**: `<ModalActions surface … />` — renders the footer chrome itself (replaces
  a manual `<ModalFooter>` button row).
- **`FormWorkspace.footer`**: pass `<ModalActions … />` (no `surface`).

Cancel is `ghost`, confirm is `default` (or `destructive`). Sizing
(`h-10 min-w-[96px] px-5 py-2.5 text-sm font-semibold`) is baked in — never pass it. When
`confirm.loading` is true, a spinner shows and both buttons disable. Labels use
`t('common.buttons.actions.*')`; an omitted `cancel.label` defaults to
`common.buttons.actions.cancel`.

| Prop        | Type                                                                                                                        | Description                                                           |
| ----------- | --------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `confirm`   | `{ label; onClick?; loading?; disabled?; variant?: 'default' \| 'destructive'; type?: 'button' \| 'submit'; form?; icon? }` | Primary action. `variant='destructive'` for delete/irreversible.      |
| `cancel`    | `{ label?; onClick?; disabled?; type? }`                                                                                    | Secondary (ghost). Omit `label` ⇒ `common.buttons.actions.cancel`.    |
| `leading`   | `ReactNode`                                                                                                                 | Left-aligned auxiliary slot (progress, target path, 2nd destructive). |
| `surface`   | `boolean`                                                                                                                   | Render footer chrome (bg/border-top/padding). Portal modals only.     |
| `className` | `string`                                                                                                                    | Extra classes on the row (e.g. `rounded-b-xl` override).              |

### ModalTitle

Reusable modal title block. Use `variant="form"` for create/edit/save dialogs so the header
matches the `WorkspaceHeader` page header (boxed icon + `text-lg font-semibold`, icon box vertically
centered against the title); keep the default variant for confirm/delete/help/info dialogs.
See ADR-0089 and the [Page & Modal Headers pattern](PATTERNS.md).

**Icon must reflect the resource**, not a generic placeholder: pass the entity glyph via
`getEntityIcon(entityType)` from `@utils/icons` (dataset → database, code → code, pipeline →
stackshare, knowledge → book, collection → package). For a dialog whose type is dynamic (e.g. the
template wizard creating a dataset _or_ code), derive the icon from the active type so the icon,
title and type badge stay in sync.

| Prop            | Type                  | Description                                                                                                                                    |
| --------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `title`         | `ReactNode`           | Title text                                                                                                                                     |
| `subtitle`      | `ReactNode`           | Optional subtitle                                                                                                                              |
| `icon`          | `ReactNode`           | Optional leading icon                                                                                                                          |
| `align`         | `'start' \| 'center'` | Text alignment                                                                                                                                 |
| `variant`       | `'default' \| 'form'` | `'default'`: inline icon, `text-xl font-bold`. `'form'`: boxed icon, `text-lg font-semibold` (matches `WorkspaceHeader`). Default `'default'`. |
| `iconClassName` | `string`              | `'form'` variant only — overrides the icon box bg/text color (defaults to `bg-primary-container text-on-primary-container`)                    |

### PanelHandle

Chevron button for collapsing/expanding side panels.

| Prop        | Type                | Description        |
| ----------- | ------------------- | ------------------ |
| `direction` | `'left' \| 'right'` | Collapse direction |
| `onClick`   | `() => void`        | Toggle handler     |

### Resizer

Drag-to-resize divider bar for adjustable panel widths/heights.

| Prop        | Type                         | Description     |
| ----------- | ---------------------------- | --------------- |
| `direction` | `'horizontal' \| 'vertical'` | Resize axis     |
| `onResize`  | `(delta) => void`            | Resize callback |

### SegmentedControl

Custom radio-based segmented control (tab-like toggle group). Use for **form/editor radio groups** — a small, mutually exclusive set (2–4) where the choices should be scannable at a glance. Reserve `ListSegmentedFilter` (above) for list-page status/type/provider filters.

| Prop        | Type                           | Description               |
| ----------- | ------------------------------ | ------------------------- |
| `options`   | `Array<{value, label, icon?}>` | Segment options           |
| `value`     | `string`                       | Selected value            |
| `onChange`  | `(value) => void`              | Change handler            |
| `fullWidth` | `boolean`                      | Stretch to fill container |

The track is `bg-panel` and the selected option is `bg-surface` + `border-border-default`. Don't move the track back to `bg-surface-muted`: in dark mode that token is the same slate-800 as `bg-surface`, so the selection becomes invisible. Note the border token class is `border-border-default` — a bare `border-default` matches no rule and silently falls back to the preflight default.

**Binary / immutable resource-type field (ADR-0151).** For a resource `type` chosen once and immutable after creation (e.g. model `internal` / `external`), don't bind the control to a `Form.Item` directly. Instead hold the value in a hidden `Form.Item name="type"` and render:

- **create** — `SegmentedControl` (with an `icon` per option), `onChange={(v) => form.setFieldValue('type', v)}`;
- **edit** — a disabled `Input` showing the resolved label (prefer `model?.type` over the watched value to avoid a first-render flicker).

Keep the `extra` helper ("Cannot be changed after creation") so immutability is stated at create time.

Precedent for the segmented + hidden-`Form.Item` structure: `src/pages/models/edit.tsx` and `src/pages/settings/llm-models/edit.tsx`. The `model?.type` flicker-avoidance (edit branch) is currently applied only in `models/edit.tsx`; `llm-models/edit.tsx` still reads the watched value and so has a minor first-render flicker on edit — a candidate follow-up, not a reference for that detail.

### SubLabel

Small muted text label using Ant Design `Typography.Text`.

### SubtypeTag / DatasetSubtypeTag / CodeLanguageTag / PipelineSubtypeTag

Color-coded subtype badge with icon.

| Prop      | Type                                        | Description  |
| --------- | ------------------------------------------- | ------------ |
| `icon`    | `ReactNode`                                 | Tag icon     |
| `label`   | `string`                                    | Tag text     |
| `variant` | `'blue' \| 'orange' \| 'green' \| 'violet'` | Color scheme |

### TagInput

Enter-to-add tag input field with removable pill tags.

The field follows the responsive selected-value pattern used by Ant Design's
`Select maxTagCount="responsive"`: it reserves the input's minimum width, shows
as many leading pills as fit, and collapses the remainder into a focusable
`+N` pill. Hovering or focusing `+N` shows the omitted labels in a tooltip.
The tooltip uses the theme's elevated neutral surface and renders omitted
labels with the same pill colors as visible tags, so the relationship remains
clear while contrast stays consistent in light and dark mode.
Both horizontal and vertical scrollbars stay hidden; consumers must not add
one-off overflow behavior around the shared component.

| Prop              | Type                   | Description                                                                                                                          |
| ----------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `tags`            | `string[]`             | Current tags (stored values; usually English canonical for auto-tags)                                                                |
| `onAddTag`        | `(tag) => void`        | Add handler                                                                                                                          |
| `onRemoveTag`     | `(tag) => void`        | Remove handler                                                                                                                       |
| `readOnlyTags`    | `string[]`             | Canonical tags rendered without a remove action; use for resource auto-tags                                                          |
| `size`            | `'sm' \| 'md' \| 'lg'` | Height and text size; mirrors `Input` / `Select` (`md` = 40px with 16px horizontal padding)                                          |
| `displayLabelFor` | `(tag) => string`      | Optional. Render-time translation hook for resource auto-tags. Pair with `getAutoTagDisplayLabel` (`@utils/autoTags`). See ADR-0060. |

Stored values stay untouched (key matching, `readOnlyTags`, and the
`onAddTag` / `onRemoveTag` callbacks all see the canonical string);
only the rendered chip text is replaced.

Resource create surfaces governed by ADR-0060 initialize their canonical type
tag with `buildAutoTagsForResource`, pass the same value to `readOnlyTags`, and
translate it only through `displayLabelFor`. User-entered tags remain removable.
Edit surfaces preserve legacy records as stored and must not silently inject a
missing type tag while loading them.

### ThemeToggleAndLocaleSwitcher

Combined dark/light mode toggle + locale dropdown for the header. No props.

### WorkspaceHeader

**The single source for page-level headers** (create / edit / detail / builder screens): back
button + boxed icon + title/subtitle + right-aligned slot. `FormWorkspace` renders it internally,
so full-page forms get it for free. Never hand-roll a page header — its visual contract (icon box
`size-9 rounded-card bg-primary-container`, title `text-lg font-semibold`, subtitle
`text-sm text-text-secondary`) is the reference that `ModalTitle variant="form"` mirrors so the same
action looks identical as a page and a modal. On full-page edit screens, `title` is the resource
display name (`alias -> name -> decoded route identifier`), not an action label like `OOO 편집`.
When alias becomes the title and the original name differs, place the original name in `subtitle`;
`FormWorkspace` callers should use `getResourceDisplayName`, `getResourceNameSubtitle`, and
`getRouteResourceIdentifier` from `@utils/resourceDisplay`. See ADR-0083, ADR-0089, ADR-0130, and the
[Page & Modal Headers pattern](PATTERNS.md). Pass the resource's entity glyph via
`getEntityIcon(entityType)`, not a generic icon.

| Prop            | Type            | Description                                                                               |
| --------------- | --------------- | ----------------------------------------------------------------------------------------- |
| `title`         | `ReactNode`     | Page title; edit pages use the resource display name (`text-lg font-semibold`)            |
| `subtitle`      | `ReactNode`     | Optional second line; edit pages may show the canonical `name` when alias is the title    |
| `icon`          | `ReactNode`     | Leading glyph rendered inside the boxed icon container                                    |
| `pageKey`       | `PageHeaderKey` | Resource/meta key driving the icon box tone (ADR-0158) so an editor matches its list page |
| `iconClassName` | `string`        | Escape hatch overriding the icon box bg/text color; wins over `pageKey` and the default   |
| `backLabel`     | `string`        | Accessible label for the back button                                                      |
| `onBack`        | `() => void`    | Back-button handler                                                                       |
| `headerExtra`   | `ReactNode`     | Right-aligned slot for a badge or action button group                                     |

Icon box color precedence: `iconClassName` > `pageKey` tone > `bg-primary-container` default. `FormWorkspace` forwards `pageKey` too.

### PageHeaderIcon (`src/components/PageHeaderIcon.tsx`)

The boxed page-header icon with the shared **2-tier tone** (ADR-0158). Use it directly on list-page
headers (`WorkspaceHeader` applies the same tone via `pageKey`). Replaces the removed
`HEADER_GRADIENT` registry — no per-page gradient, `text-white`, or shadow. `getPageHeaderIconTreatment(pageKey, isDark)`
resolves the box: **Tier 1 (resource types)** — `collection`/`dataset`/`code`/`pipeline`/`knowledge`/`dashboard`/`entity`/`relation`/`agent`/`tool`/`actor` — a flat `getResourceColor()` tint that matches the resource's badge/tree/row and is identical on the list and its editor; **Tier 2 (meta/admin: settings, users, groups, secrets, markings, oidc, connectors, recents, labs, examples, home, prompts, semantic layer)** — a single neutral `bg-subtle text-text-secondary`. The box shape is fixed (`size-9 rounded-card`); the icon glyph stays explicit. Reads theme defensively (renders outside a `ConfigProvider`, defaults to light). **Import via the subpath `@components/PageHeaderIcon`, not the `@components` barrel** (the barrel pulls unrelated modules into partial-mock page tests). See the [Header icon box tone pattern](PATTERNS.md).

```tsx
<PageHeaderIcon pageKey="knowledge" className="mt-0.5">
  <IconBook2 size={20} aria-hidden="true" />
</PageHeaderIcon>
```

| Prop        | Type            | Description                                                      |
| ----------- | --------------- | ---------------------------------------------------------------- |
| `pageKey`   | `PageHeaderKey` | Resource type (Tier 1 tint) or meta page key (Tier 2 neutral)    |
| `children`  | `ReactNode`     | The icon element (inherits color via `currentColor`)             |
| `className` | `string`        | Outer positioning only (e.g. `mt-0.5`); box shape/color is fixed |

### ListPageHeader (`src/components/ListPageHeader.tsx`)

The **sticky list-page header shell** (ADR-0236) — the wrapper around `PageHeaderIcon`, the title, the subtitle, and the right-hand action slot. Owns `sticky top-0 z-20`, `px-8 py-6`, `border-b border-border-default`, the `mt-0.5` icon alignment, and the `text-2xl font-bold text-text-primary leading-tight` title, and applies `aria-hidden` to the glyph so no call site has to remember it. The parts were already shared; the layout was not, and 34 files carried it as a copied class string — the border split 20/14 between `slate-*` and the token, the title color 12/22, with three background treatments and two files missing `sticky`. `lint:colors` counts only hex, so none of it was caught. **Import via the subpath `@components/ListPageHeader`** (same barrel caveat as `PageHeaderIcon`). Scope is the header only: list bodies vary too much (toolbars, bulk bars, split panes) for a `FormWorkspace`-style page shell. The ground is fixed at `--color-surface` and is **not** configurable — the header sits on the same plane as the content cards it heads and as the editor chrome, so a per-page override would only reintroduce drift (ADR-0236 §8). Enforced by `src/components/__tests__/listPageHeaderShell.coverage.test.ts`, which scans for `PageHeaderIcon` beside a `text-2xl font-bold` heading; unmigrated pages sit in a frozen `PENDING_MIGRATION` list that only shrinks (BACKLOG DX-004), separate from the permanent `STRUCTURAL_EXEMPT` list.

```tsx
<ListPageHeader
  pageKey="replications"
  icon={<IconTransfer size={20} />}
  title={t('replications.title')}
  subtitle={t('replications.subtitle')}
  actions={
    <Button variant="default" className={LIST_PRIMARY_CTA_CLASS} icon={<IconPlus size={16} />}>
      {t('replications.create.cta')}
    </Button>
  }
/>
```

| Prop       | Type            | Description                                                 |
| ---------- | --------------- | ----------------------------------------------------------- |
| `pageKey`  | `PageHeaderKey` | Icon box tone (ADR-0158); same key as the resource's editor |
| `icon`     | `ReactNode`     | Header glyph; `aria-hidden` is applied by the component     |
| `title`    | `ReactNode`     | Page title                                                  |
| `subtitle` | `ReactNode`     | Label-style fragment, no trailing period                    |
| `actions`  | `ReactNode`     | Right-hand slot: primary CTA, refresh, overflow menu        |

### InputParameterEditor (`src/components/InputParameterEditor/`)

**The shared editor for invocation/input definitions** used by Tool and Actor Python input
parameters plus the workflow Start and Human Input nodes. It renders a persistent
**Name → Type → Description → Required → Actions** grid when its container is wide enough and
container-adapts to a labeled stacked form in narrow inspector panels. Import it from the direct
subpath `@components/InputParameterEditor`.

| Prop                   | Type                     | Description                                                     |
| ---------------------- | ------------------------ | --------------------------------------------------------------- |
| `parameters`           | `TParameter[]`           | Controlled parameter definitions                                |
| `onChange`             | `(parameters) => void`   | Update handler; omit for read-only rendering                    |
| `createParameter`      | `() => TParameter`       | Domain-specific default row factory                             |
| `types`                | `TType[]`                | Allowed type values; labels resolve through common i18n keys    |
| `density`              | `'default' \| 'compact'` | Compact spacing for workflow inspectors                         |
| `showDescription`      | `boolean`                | Hides the description column for field definitions that lack it |
| `showValidationErrors` | `boolean`                | Reveals empty/duplicate-name errors after a parent save attempt |
| `nameLabel` etc.       | `string`                 | Domain wording overrides such as Variable name or Field name    |
| `countLabel`           | `string`                 | Domain-specific count copy such as input variables or fields    |
| `readOnly`             | `boolean`                | Disables fields and removes add/delete actions                  |

`getInputParameterNameIssues()` and `hasInputParameterIssues()` provide the matching save guard.
The editor keeps all controls programmatically labeled, always exposes the delete action, and uses
semantic theme tokens in both themes. It does **not** edit JSON Schema `enum`, `default`, `items`,
or nested `properties`; those require a separate data-contract enhancement.

### SchemaEditor / SchemaUIMode / TypePicker (ADR-0139)

**The single table-schema editor** for every data-table schema surface — ontology entity/relation modals & inspectors, workflow attributes tab, dataset-create wizard, and (via the `DatasetSchemaEditor` wrapper) the dataset detail Schema tab. Invocation/input definitions use `InputParameterEditor` instead. `SchemaEditor` owns the control bar + `Table`/JSON/CSV mode switch and renders `SchemaUIMode`, the CSS-grid row list. Never build a second data-table schema editor — the old `EditableSchemaTab` was deleted. Full behavior + rules: [Schema / Attributes Editor pattern](PATTERNS.md §13).

Implementation: `src/components/schema/`. Shared schema contracts and key serialization live with the component module; feature pages must not import an implementation from another page directory.

- **`SchemaTypeIcon`** — data-type-family icon tinted via `getSchemaTypeColor` (`--color-schema-type-*` tokens). `getSchemaTypeMeta(dataType)` resolves the category/icon (`src/utils/schemaType.ts`).
- **`TypePicker`** — grouped, searchable type combobox (type-family icons + native hints) replacing the flat Select.

Key `SchemaUIMode`/`SchemaEditor` props:

| Prop                                        | Type                      | Description                                                       |
| ------------------------------------------- | ------------------------- | ----------------------------------------------------------------- |
| `schemaRows`                                | `SchemaRow[]`             | Row model; edited via `onRowsChange` / `onAddRow` / `onDeleteRow` |
| `lockedRowIds`                              | `ReadonlySet<string>`     | Persisted rows whose name/type/nullable/PK/delete are locked      |
| `identityKeyNames` / `onIdentityKeysChange` | ordered `string[]`        | Inline PK membership + order (ontology only; omit for datasets)   |
| `jsonPathEditable`                          | `boolean`                 | REST: turns the JSONPath marker into a click-to-edit popover      |
| `showReservedFields`                        | `boolean`                 | Treat `id` as reserved (ontology `true`, datasets `false`)        |
| `entityContext`                             | `{ name, alias?, tags? }` | Feeds per-row AI alias/description generation                     |
| `availableModes`                            | `SchemaMode[]`            | Hide unused JSON/CSV modes (e.g. `['ui']`)                        |
| `density`                                   | `'default' \| 'compact'`  | Compact for narrow inspector surfaces                             |

## Single-purpose shared components

These are shared because more than one page mounts them, not because there is a choice to make — they are listed so the catalog matches the directory, and they do not need a Decision Matrix row.

| Component                       | Purpose                                                                                                                                                                  |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `AskAssistantButton`            | Secondary "create with assistant" entry, always left of the primary create CTA — never replaces it (ADR-0193). Carries `AIAssistantEntryIcon` per the ADR-0065 amendment |
| `AssistantCompanion`            | Draggable assistant sprite with persisted dock position and post-turn poses (ADR-0197)                                                                                   |
| `CleanupPanelDock`              | Bottom-right stack of in-flight `ResourceCleanupPanel`s (ADR-0010)                                                                                                       |
| `ResourceCleanupPanel`          | Progress panel for one async resource cleanup job                                                                                                                        |
| `CollectionItemBulkDeleteModal` | Bulk delete confirmation for selected collection items                                                                                                                   |
| `KnowledgeDeleteConflictModal`  | 409 conflict dialog offering cascade delete when a knowledge still has documents                                                                                         |
| `UpdateAvailableBanner`         | Non-blocking new-build banner; never auto-reloads, so unsaved editor work survives (ADR-0071)                                                                            |
| `VisualizationBlocks`           | Renders every `visualize_*` block in an assistant/agent message (charts, scatter, table, map, knowledge graph). Import from `@components/ai/visualizations`              |
| `AddToDashboardDialog`          | Promotes a `dataRef`-carrying chat block to a dashboard widget, scoped to the source's collection (ADR-0225 §2.7). Mounted by `ChatDataRefBar`, not imported directly    |
| `AprilFoolsEasterEgg`           | Date-gated easter egg                                                                                                                                                    |
