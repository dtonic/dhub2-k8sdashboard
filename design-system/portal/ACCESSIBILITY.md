# Accessibility Guide

WCAG 2.1 Level AA accessibility requirements for D.Hub2 Portal. This guide covers color contrast, keyboard navigation, screen reader support, and component-specific requirements.

---

## 1. WCAG 2.1 AA Checklist

### Perceivable

| Criterion                    | Level | Requirement                               | Status                                                                           |
| ---------------------------- | ----- | ----------------------------------------- | -------------------------------------------------------------------------------- |
| 1.1.1 Non-text Content       | A     | All images, icons have text alternatives  | Partial                                                                          |
| 1.3.1 Info and Relationships | A     | Semantic HTML, ARIA roles for structure   | Partial                                                                          |
| 1.3.2 Meaningful Sequence    | A     | DOM order matches visual order            | Good                                                                             |
| 1.4.1 Use of Color           | A     | Color is not the only indicator of state  | Partial                                                                          |
| 1.4.3 Contrast (Minimum)     | AA    | 4.5:1 for normal text, 3:1 for large text | Partial — text-tier tokens AA both modes (ADR-0147/0148); full app audit pending |
| 1.4.4 Resize Text            | AA    | Content readable at 200% zoom             | Good                                                                             |
| 1.4.11 Non-text Contrast     | AA    | 3:1 for UI components and graphics        | Needs audit                                                                      |

### Operable

| Criterion              | Level | Requirement                              | Status  |
| ---------------------- | ----- | ---------------------------------------- | ------- |
| 2.1.1 Keyboard         | A     | All functionality available via keyboard | Partial |
| 2.1.2 No Keyboard Trap | A     | Focus can always be moved away           | Good    |
| 2.4.1 Bypass Blocks    | A     | Skip navigation links                    | Missing |
| 2.4.3 Focus Order      | A     | Logical tab order                        | Good    |
| 2.4.4 Link Purpose     | A     | Link text describes destination          | Good    |
| 2.4.7 Focus Visible    | AA    | Visible focus indicator                  | Partial |

### Understandable

| Criterion                    | Level | Requirement                           | Status |
| ---------------------------- | ----- | ------------------------------------- | ------ |
| 3.1.1 Language of Page       | A     | `lang` attribute on `<html>`          | Good   |
| 3.2.1 On Focus               | A     | No unexpected context change on focus | Good   |
| 3.3.1 Error Identification   | A     | Errors clearly described              | Good   |
| 3.3.2 Labels or Instructions | A     | Form inputs have labels               | Good   |

### Robust

| Criterion               | Level | Requirement           | Status  |
| ----------------------- | ----- | --------------------- | ------- |
| 4.1.1 Parsing           | A     | Valid HTML            | Good    |
| 4.1.2 Name, Role, Value | A     | ARIA roles and states | Partial |

---

## 2. Color Contrast Requirements

### Minimum Ratios

| Element                            | Ratio Required | Standard    |
| ---------------------------------- | -------------- | ----------- |
| Normal text (< 18px, < 14px bold)  | 4.5:1          | WCAG AA     |
| Large text (>= 18px, >= 14px bold) | 3:1            | WCAG AA     |
| UI components (borders, icons)     | 3:1            | WCAG 2.1 AA |
| Decorative elements                | No requirement | —           |

### Token Contrast Guide

When choosing token combinations, ensure minimum contrast:

| Background Token                  | Text Token                                | Pair                         |
| --------------------------------- | ----------------------------------------- | ---------------------------- |
| `--color-surface` (light: #fff)   | `--color-text-primary` (light: #1f2937)   | High contrast (~12:1)        |
| `--color-surface` (light: #fff)   | `--color-text-secondary` (light: #4b5563) | Meets AA (7.55:1)            |
| `--color-surface` (light: #fff)   | `--color-text-tertiary` (light: #6b7280)  | Meets AA (4.57:1) — ADR-0148 |
| `--color-surface` (dark: #1e293b) | `--color-text-primary` (dark: #f8fafc)    | High contrast (~14:1)        |
| `--color-surface` (dark: #1e293b) | `--color-text-secondary` (dark: #94a3b8)  | Meets AA (5.3:1)             |
| `--color-surface` (dark: #1e293b) | `--color-text-tertiary` (dark: #8a96ad)   | Meets AA (4.9:1) — ADR-0147  |

> **Surface dependency**: the tertiary ratios above are measured on `--color-surface` (light `#fff`, dark slate-800 `#1e293b`). On darker light surfaces (panel `#f3f4f6` ≈ 4.0:1) or lighter dark surfaces the tertiary tier dips below 4.5:1 — use `--color-text-secondary` for essential small body copy. See ADR-0147 (dark) and ADR-0148 (light).

### Validation

Use browser DevTools (Chrome: Inspect > Accessibility pane) or the [WebAIM Contrast Checker](https://webaim.org/resources/contrastchecker/) to verify contrast ratios when introducing new color combinations.

---

## 3. Component Accessibility Requirements

### Button

```tsx
// Icon-only buttons MUST have aria-label
<Button variant="icon" aria-label={t('common.delete')}>
  <IconTrash size={16} />
</Button>

// Loading buttons should announce state
<Button loading aria-busy={true} aria-label={t('common.saving')}>
  {t('common.save')}
</Button>

// Disabled buttons should explain why (tooltip)
<Tooltip title={t('common.requiredFieldsMissing')}>
  <Button disabled>{t('common.submit')}</Button>
</Tooltip>
```

### Modal / Dialog

Ant Design `Modal` handles most a11y concerns automatically. Ensure:

```tsx
<Modal
  open={isOpen}
  onCancel={onClose}
  title={t('common.confirmDelete')}     // Announces modal title
  destroyOnClose                         // Removes from DOM when closed
  // Ant Design auto-handles: aria-modal, focus trap, Escape key
>
```

- Focus returns to the trigger element when modal closes
- Modal content is keyboard-navigable
- `destroyOnClose` ensures screen readers don't find hidden modal content

### Form

```tsx
<FormField
  label={t('common.email')}
  required // Adds aria-required via FormLabel
  error={errors.email} // Renders error with aria-describedby linkage
  htmlFor="email-input" // Links label to input
>
  <Input
    id="email-input"
    aria-invalid={!!errors.email} // Announces validation state
    aria-describedby="email-error" // Links to error message
  />
</FormField>
```

### Status Indicators

Status changes should be announced to screen readers:

```tsx
// Use aria-live for dynamic status updates
<div aria-live="polite" aria-atomic="true">
  <StatusBadge status={pipelineStatus} />
  <span className="sr-only">{t('pipeline.statusChanged', { status: pipelineStatus })}</span>
</div>
```

### Table

Ant Design `Table` handles basic table semantics. Enhance with:

```tsx
<Table
  aria-label={t('datasets.tableLabel')} // Table purpose
  columns={columns}
  dataSource={data}
  rowKey="id"
  // For sortable columns, Ant Design adds aria-sort automatically
/>
```

### Tree

```tsx
<Tree
  aria-label={t('collections.treeLabel')}
  treeData={nodes}
  // Ant Design handles: aria-expanded, role="tree", role="treeitem"
/>
```

### Tabs

```tsx
<Tabs
  aria-label={t('settings.tabsLabel')}
  items={tabItems}
  // Ant Design handles: role="tablist", role="tab", role="tabpanel", aria-selected
/>
```

---

## 4. Keyboard Navigation

### Global Shortcuts

| Shortcut          | Action                                                |
| ----------------- | ----------------------------------------------------- |
| `Tab`             | Move focus forward                                    |
| `Shift + Tab`     | Move focus backward                                   |
| `Escape`          | Close modal/dropdown/popover                          |
| `Enter` / `Space` | Activate focused element                              |
| `Arrow keys`      | Navigate within composite widgets (tabs, tree, menus) |

### Focus Management Rules

1. **Visible focus indicator**: All interactive elements must show a visible focus ring. Use `--focus-ring-color` token.
2. **Logical tab order**: DOM order should match visual order. Avoid `tabIndex` values greater than 0.
3. **Focus trap in modals**: Ant Design `Modal` handles this automatically.
4. **Focus restoration**: When a modal/popover closes, focus returns to the trigger element.
5. **Skip navigation**: Consider adding a skip link for the main content area:

```tsx
<a href="#main-content" className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:p-4">
  {t('common.skipToContent')}
</a>
```

### Focus Ring Styling

The focus ring uses `--focus-ring-color` (defaults to `--color-primary-600`):

```css
*:focus-visible {
  outline: 2px solid var(--focus-ring-color);
  outline-offset: 2px;
}
```

---

## 5. Screen Reader Utilities

### `sr-only` Class

Use the `sr-only` class (from Tailwind) to provide text that's only visible to screen readers:

```tsx
// Icon-only elements
<button>
  <IconSearch size={16} />
  <span className="sr-only">{t('common.labels.search')}</span>
</button>

// Decorative icons with adjacent text (no sr-only needed)
<button>
  <IconPlus size={16} />
  {t('common.create')}
</button>

// Status indicators that use only color
<div className="w-2 h-2 rounded-full bg-green-500" />
<span className="sr-only">{t('common.online')}</span>
```

### When to Use `sr-only`

| Scenario                    | Action                                  |
| --------------------------- | --------------------------------------- |
| Icon-only button            | Add `aria-label` OR `sr-only` text      |
| Color-only status indicator | Add `sr-only` text describing the state |
| Visual separator/divider    | No action needed (decorative)           |
| Chart/graph                 | Add `aria-label` with data summary      |
| Loading spinner             | Add `aria-busy="true"` on the container |

### When NOT to Use `sr-only`

- Text that's already visible — redundant for screen readers
- Decorative images/icons next to descriptive text
- Elements hidden with `aria-hidden="true"` — they're already invisible to AT

---

## 6. ARIA Attributes Quick Reference

| Attribute          | When to Use                                       | Example                                  |
| ------------------ | ------------------------------------------------- | ---------------------------------------- |
| `aria-label`       | Labels element with no visible text               | `<button aria-label="Close">`            |
| `aria-labelledby`  | Labels element using another element's text       | `<div aria-labelledby="heading-id">`     |
| `aria-describedby` | Additional description (error message, help text) | `<input aria-describedby="name-error">`  |
| `aria-invalid`     | Form input has validation error                   | `<input aria-invalid={!!error}>`         |
| `aria-required`    | Form input is required                            | `<input aria-required="true">`           |
| `aria-expanded`    | Toggle button controls expandable content         | `<button aria-expanded={isOpen}>`        |
| `aria-live`        | Region content updates dynamically                | `<div aria-live="polite">`               |
| `aria-busy`        | Content is loading/updating                       | `<div aria-busy={isLoading}>`            |
| `aria-hidden`      | Element is decorative, hide from AT               | `<span aria-hidden="true">•</span>`      |
| `role="status"`    | Live region for status messages                   | `<div role="status">Saved</div>`         |
| `role="alert"`     | Urgent live region for errors                     | `<div role="alert">Error occurred</div>` |

---

## 7. Motion & Reduced Motion

Respect the `prefers-reduced-motion` media query for users who have enabled reduced motion in their OS settings.

```tsx
// Check in JavaScript
const prefersReducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// framer-motion integration
<motion.div
  initial={{ opacity: 0 }}
  animate={{ opacity: 1 }}
  transition={{
    duration: prefersReducedMotion ? 0 : 0.25,
  }}
>
```

The global CSS in `theme.css` should include:

```css
@media (prefers-reduced-motion: reduce) {
  *,
  *::before,
  *::after {
    animation-duration: 0.01ms !important;
    animation-iteration-count: 1 !important;
    transition-duration: 0.01ms !important;
    scroll-behavior: auto !important;
  }
}
```

---

## 8. Testing Accessibility

### Manual Testing Checklist

1. **Keyboard-only navigation**: Unplug your mouse, navigate the entire page using only Tab, Enter, Escape, and arrow keys.
2. **Screen reader test**: Use VoiceOver (macOS: Cmd+F5) or NVDA (Windows) to navigate the page.
3. **Zoom test**: Set browser zoom to 200% — is content still usable?
4. **Color blindness**: Use Chrome DevTools > Rendering > Emulate vision deficiencies.
5. **Reduced motion**: Enable reduced motion in OS settings and verify animations are suppressed.

### Automated Tools

- **axe DevTools** (browser extension): Catches WCAG violations automatically
- **Lighthouse** (Chrome DevTools > Lighthouse > Accessibility): Scored audit
- **eslint-plugin-jsx-a11y** (if added to ESLint config): Catches missing aria attributes in JSX at lint time
