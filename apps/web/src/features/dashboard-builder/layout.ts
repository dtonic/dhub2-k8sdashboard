import type { DashboardDefinition, DashboardLayout, DashboardWidget } from "@k8s-dashboard/dashboard-schema";

export function placeWidget(widgets: readonly DashboardWidget[], w = 4, h = 3): DashboardLayout | null {
  for (let y = 0; y <= 96 - h; y++) for (let x = 0; x <= 12 - w; x++) {
    const next = { x, y, w, h }; if (layoutAllowed(widgets, "", next)) return next;
  }
  return null;
}

export function updateWidgetLayout(definition: DashboardDefinition, id: string, next: DashboardLayout): DashboardDefinition | null {
  if (!layoutAllowed(definition.widgets, id, next)) return null;
  return { ...definition, widgets: definition.widgets.map((w) => w.id === id ? { ...w, layout: next } as DashboardWidget : w) };
}

export function layoutAllowed(widgets: readonly DashboardWidget[], id: string, a: DashboardLayout): boolean {
  if (![a.x,a.y,a.w,a.h].every(Number.isInteger) || a.x<0 || a.y<0 || a.w<1 || a.h<1 || a.x+a.w>12 || a.y+a.h>96) return false;
  return widgets.every((widget) => widget.id === id || !overlaps(a, widget.layout));
}
const overlaps = (a: DashboardLayout,b: DashboardLayout) => a.x < b.x+b.w && a.x+a.w > b.x && a.y < b.y+b.h && a.y+a.h > b.y;
