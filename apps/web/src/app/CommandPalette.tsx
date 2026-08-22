import { useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useLocation, useNavigate } from "react-router-dom";
import type { ResourceRecentItem, ResourceSearchItem } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import {
  normalizeSearchQuery,
  recentRefsKey,
  searchKeys,
  SEARCH_MAX_QUERY,
  SEARCH_MAX_RESULTS,
  SEARCH_MIN_QUERY,
  searchQueryUsable,
  useRecentResources,
  useResourceSearch,
} from "@/api/queries";
import { focusablesIn, modalOpen } from "@/app/keyboard";
import { navRoutes, type AppRoute, type RouteCapabilities } from "@/app/routes";
import { loadRecent, pruneRecent, rememberRecent, type RecentRef } from "@/app/recent";

/**
 * Command Palette (ADR 0023)
 * --------------------------------------------------------------------------
 * 키보드 하나로 화면을 옮기고 리소스를 찾는 진입점입니다.
 *
 * - **결과의 출처는 BFF 하나입니다.** Kubernetes를 직접 부르지 않고 폴링하지도
 *   않습니다. 입력이 멈춘 뒤 한 번만 나가고, 다음 입력은 앞선 요청을 취소합니다.
 * - **지어낸 상태를 만들지 않습니다.** 검색 인덱스에는 status가 없으므로 결과에
 *   Running/Ready 같은 값을 붙이지 않습니다. kind·이름·namespace가 전부입니다.
 * - **빈 결과·동기화 중·기능 없음·부분 색인·오류를 구분합니다.** 다섯 가지를 같은
 *   "결과 없음"으로 접으면 사용자가 원인을 오판합니다.
 * - 리소스 검색 그룹은 서버가 준 `canExploreResources`로만 열립니다. 이동 그룹은
 *   기존 nav와 **같은 규칙**을 그대로 씁니다.
 */

/** 입력이 멈춘 뒤 요청까지의 대기. 타이핑마다 부르면 폴링과 다르지 않습니다. */
const DEBOUNCE_MS = 200;

/** 이동 그룹에 보여줄 최대 항목 수. 목록 전체가 한 화면에 들어와야 합니다. */
const MAX_NAV_RESULTS = 8;

interface RouteRow {
  kind: "route";
  id: string;
  route: AppRoute;
}
interface ResourceRow {
  kind: "resource";
  id: string;
  item: ResourceSearchItem;
  recent: boolean;
}
type Row = RouteRow | ResourceRow;

/** 결과가 어떤 필드에 걸렸는지. 색이 아니라 텍스트로 함께 보여줍니다. */
const MATCH_LABEL: Record<string, string> = {
  name: "이름",
  namespace: "Namespace",
  kind: "종류",
  label: "Label",
};

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(timer);
  }, [value, ms]);
  return debounced;
}

/** 검색 그룹의 상태. 빈 화면 하나로 접지 않기 위해 사유를 나눕니다. */
type SearchState = "idle" | "short" | "loading" | "ready" | "empty" | "syncing" | "unavailable" | "error";

/** 최근 항목 그룹의 상태. 검색과 실패 사유가 다르므로 따로 셉니다. */
type RecentState = "loading" | "ready" | "empty" | "unavailable" | "error";

/** 오류 코드를 사유로 분류합니다. 계약에 아직 없는 코드도 넓혀서 읽습니다. */
function classify(error: unknown): "unavailable" | "syncing" | "error" | null {
  if (!error) return null;
  if (error instanceof HttpError) {
    /* 검색만 꺼진 배포와 Explorer 자체가 없는 배포를 구분합니다. (ADR 0023 롤백) */
    const code = String(error.body.code);
    if (code === "search_unavailable" || code === "resources_unavailable") return "unavailable";
    if (code === "resource_syncing") return "syncing";
  }
  return "error";
}

function searchStateOf(args: {
  enabled: boolean;
  /** 지금 화면에 있는 입력을 정규화한 값. 디바운스된 값이 아닙니다. */
  normalized: string;
  /** 손에 든 결과가 `normalized`의 것인지. 아니면 낡은 값입니다. */
  fresh: boolean;
  loading: boolean;
  error: unknown;
  count: number;
  settled: boolean;
}): SearchState {
  if (!args.enabled) return "unavailable";
  if (args.normalized.length === 0) return "idle";
  if (args.normalized.length < SEARCH_MIN_QUERY) return "short";
  /* 디바운스가 아직 따라오지 못했습니다. 손에 든 값은 이전 입력의 것이므로
     "찾는 중"이라고 말합니다 — 낡은 결과를 최신처럼 보여주지 않습니다. */
  if (!args.fresh) return "loading";
  const kind = classify(args.error);
  if (kind === "unavailable") return "unavailable";
  if (kind === "syncing") return "syncing";
  if (kind === "error") return "error";
  if (args.loading || !args.settled) return "loading";
  return args.count === 0 ? "empty" : "ready";
}

function recentStateOf(args: { enabled: boolean; fetching: boolean; error: unknown; settled: boolean; count: number }): RecentState {
  if (!args.enabled) return "unavailable";
  const kind = classify(args.error);
  /* 최근 항목은 "동기화 중"도 지금은 결과를 못 준다는 뜻이라 오류로 묶지 않고
     기다림으로 보여줍니다. 그 외 실패는 조용히 빈 목록으로 만들지 않습니다. */
  if (kind === "unavailable") return "unavailable";
  if (kind === "syncing") return "loading";
  if (kind === "error") return "error";
  if (args.fetching || !args.settled) return "loading";
  return args.count === 0 ? "empty" : "ready";
}

export interface CommandPaletteProps {
  clusterId: string;
  caps: RouteCapabilities;
  /** nav 이동 시 유지할 공용 쿼리(cluster·범위). AppShell이 계산해 넘깁니다. */
  navSearch: string;
}

/** 아직 저장소를 읽지 않았을 때의 참조 목록. 신원이 고정되어야 효과가 헛돌지 않습니다. */
const NO_REFS: RecentRef[] = [];

export function CommandPalette({ clusterId, caps, navSearch }: CommandPaletteProps) {
  const [open, setOpen] = useState(false);
  const [raw, setRaw] = useState("");
  const [active, setActive] = useState(0);
  /**
   * 이번 열기에서 읽어 둔 최근 참조. `null`은 **아직 안 읽음**입니다.
   *
   * 이 장벽이 없으면 열자마자 `refs=[]`로 조회가 시작되어, 저장된 항목이 있는데도
   * 0건 probe가 한 번 헛나가고 곧바로 두 번째 질의가 따라붙습니다. 사용자에게는
   * "최근이 비었다"가 한 프레임 보였다가 바뀌고, 서버에는 요청이 하나 더 갑니다.
   *
   * 참조와 epoch를 **한 덩어리로** 들고 있어 중간 상태(ready인데 refs는 옛것)가
   * 생기지 않습니다. epoch는 열 때마다 올라가 캐시 키를 갈아치우므로, 다시 열면
   * 반드시 다시 물어보고 그 답이 오기 전까지 옛 제목이 남지 않습니다.
   */
  const [recentLoad, setRecentLoad] = useState<{ epoch: number; clusterId: string; refs: RecentRef[] } | null>(null);
  const epochRef = useRef(0);
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const inputRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreRef = useRef<HTMLElement | null>(null);
  const baseId = useId();

  /* 화면의 입력과 디바운스된 질의를 **같은 규칙**으로 정규화합니다. 이 둘이
     같을 때만 손에 든 결과가 지금 입력의 것입니다. */
  const normalized = normalizeSearchQuery(raw);
  const query = useDebounced(normalized, DEBOUNCE_MS);
  const canExplore = caps.canExploreResources;
  const isRecentMode = normalized.length === 0;

  /* 장벽은 **열기 회차와 클러스터 둘 다**에 묶입니다. 클러스터만 보면 다시 열어도
     옛 응답이 살아나고, 회차만 보면 클러스터를 바꾼 순간 옛 클러스터의 참조가
     새 클러스터 엔드포인트로 나갑니다. */
  const recentReady = recentLoad !== null && recentLoad.clusterId === clusterId;
  const refs = recentReady ? recentLoad.refs : NO_REFS;
  const recentEpoch = recentReady ? recentLoad.epoch : 0;

  /* 2자 미만은 아예 보내지 않습니다. 서버도 거절하지만, 거절받으려고 나가는 요청은
     타이핑 한 글자마다 왕복 한 번을 그냥 버리는 것입니다. */
  const searchEnabled = open && canExplore && searchQueryUsable(query);
  /* **이 클러스터의 저장소를 다 읽은 뒤에만** 조회합니다. */
  const recentEnabled = open && canExplore && isRecentMode && recentReady;
  const search = useResourceSearch(clusterId, query, searchEnabled);
  const recent = useRecentResources(clusterId, refs, recentEnabled, recentEpoch);

  /** 손에 든 검색 결과가 지금 입력의 것인지. 디바운스 대기 중에는 아닙니다. */
  const fresh = query === normalized;
  const searchData = fresh ? search.data : undefined;
  const searchError = fresh ? search.error : undefined;

  /* ── 취소 ───────────────────────────────────────────────────────────────
     `enabled: false`만으로는 부족합니다. QueryObserver는 마운트된 채로 남고
     이미 나간 fetch는 계속 달립니다. 그래서 **키를 정확히 지목해** 끊습니다 —
     `exact: true`라 다른 화면의 질의는 건드리지 않습니다. */
  const searchKeyOf = useCallback(
    (q: string) => searchKeys.search(clusterId, q, ""),
    [clusterId],
  );
  /** 지금 나가 있는 검색 질의어. 취소 대상은 이 하나뿐입니다. */
  const inflightSearch = useRef<string | null>(null);
  const inflightRecent = useRef<readonly unknown[] | null>(null);

  useEffect(() => {
    if (searchEnabled && query === normalized) inflightSearch.current = query;
  }, [searchEnabled, query, normalized]);

  useEffect(() => {
    if (recentEnabled) inflightRecent.current = searchKeys.recent(clusterId, recentRefsKey(refs), recentEpoch);
  }, [recentEnabled, clusterId, refs, recentEpoch]);

  /* 입력이 앞선 질의를 낡게 만들면 **디바운스를 기다리지 않고** 그 요청을 끊습니다. */
  useEffect(() => {
    const issued = inflightSearch.current;
    if (issued === null) return;
    const wanted = open ? normalized : null;
    if (issued === wanted) return;
    inflightSearch.current = null;
    void queryClient.cancelQueries({ queryKey: searchKeyOf(issued), exact: true });
  }, [normalized, open, queryClient, searchKeyOf]);

  /**
   * 최근 항목 조회가 더 이상 필요 없어진 순간 그 요청을 끊습니다.
   *
   * 닫기만이 아니라 **입력을 시작해 검색으로 넘어갈 때와 클러스터를 바꿀 때**도
   * 포함합니다 — 그때부터 그 응답은 화면에 쓰이지 않으므로, 계속 달리게 두면
   * 아무도 기다리지 않는 요청이 남습니다. 신호를 모든 덩어리가 공유하므로 남은
   * 덩어리까지 함께 멈춥니다.
   */
  useEffect(() => {
    if (recentEnabled) return;
    const key = inflightRecent.current;
    if (!key) return;
    inflightRecent.current = null;
    void queryClient.cancelQueries({ queryKey: key, exact: true });
  }, [recentEnabled, queryClient]);

  /* 언마운트도 닫힘과 같습니다. 남긴 요청이 백그라운드에서 계속 달리지 않습니다. */
  useEffect(
    () => () => {
      const s = inflightSearch.current;
      const r = inflightRecent.current;
      if (s !== null) void queryClient.cancelQueries({ queryKey: searchKeyOf(s), exact: true });
      if (r) void queryClient.cancelQueries({ queryKey: r, exact: true });
    },
    [queryClient, searchKeyOf],
  );

  /* 팔레트를 열 때만 저장소를 읽습니다 — 렌더 중에 읽으면 매 렌더마다 다른 탭의
     변경과 경쟁하면서도 이득이 없고, 렌더를 순수하지 않게 만듭니다.
     닫으면 장벽을 되돌려 다음 열기에서 반드시 다시 읽고 다시 물어봅니다. */
  useEffect(() => {
    if (!open) {
      setRecentLoad(null);
      return;
    }
    epochRef.current += 1;
    setRecentLoad({ epoch: epochRef.current, clusterId, refs: loadRecent(clusterId) });
  }, [open, clusterId]);

  /** 재확인이 끝난 값만 씁니다. 도는 동안 옛 캐시를 보여주면 권한 잃은 항목이 남습니다. */
  const recentResolved = recentEnabled && recent.isSuccess && !recent.isFetching ? recent.data : undefined;

  /**
   * 서버가 해석하지 못한 참조(삭제·권한 상실·교체)는 저장소에서도 지웁니다.
   *
   * **완전히 성공한 해석 뒤에만** 지웁니다 — 중간에 끊긴 결과로 지우면 살아 있는
   * 항목을 잃습니다. 그리고 **이번 열기의 질의 키는 건드리지 않습니다.** 정리 결과로
   * `refs`를 줄이면 키가 바뀌어 방금 받은 응답을 버리고 다시 물어보게 되고,
   * 전부 사라진 경우에는 0건 probe가 뒤따라 나갑니다. 화면은 이미 받은 응답을
   * 그대로 계속 보여주고, 줄어든 목록은 다음에 열 때 반영됩니다.
   */
  useEffect(() => {
    if (!recentResolved) return;
    pruneRecent(refs, recentResolved);
  }, [recentResolved, refs]);

  const routeRows: RouteRow[] = useMemo(() => {
    const needle = normalized.toLowerCase();
    return navRoutes(caps)
      .filter((r) => (needle ? r.label.toLowerCase().includes(needle) || r.path.includes(needle) : true))
      .slice(0, MAX_NAV_RESULTS)
      .map((route) => ({ kind: "route" as const, id: `route:${route.id}`, route }));
  }, [caps, normalized]);

  const resourceRows: ResourceRow[] = useMemo(() => {
    if (!canExplore) return [];
    const source: Array<ResourceRecentItem | ResourceSearchItem> = isRecentMode
      ? (recentResolved ?? [])
      : (searchData?.items ?? []);
    return source.slice(0, SEARCH_MAX_RESULTS).map((item) => ({
      kind: "resource" as const,
      id: `res:${item.group}/${item.version}/${item.resource}/${item.uid}`,
      /* 최근 항목에는 매칭 필드가 없습니다. 검색과 같은 모양으로 맞추되 값을
         지어내지 않고 이름으로 둡니다 — 화면에도 "최근"으로만 표시됩니다. */
      item: ("matchedField" in item ? item : { ...item, matchedField: "name" }) as ResourceSearchItem,
      recent: isRecentMode,
    }));
  }, [canExplore, isRecentMode, recentResolved, searchData]);

  const rows = useMemo(() => [...routeRows, ...resourceRows], [routeRows, resourceRows]);

  /* 결과가 바뀌면 선택을 첫 항목으로 되돌립니다. 화면에 없는 항목이 선택된 채로
     남으면 Enter가 예상 밖으로 동작합니다. */
  useEffect(() => {
    setActive(0);
  }, [rows.length, normalized]);

  const close = useCallback(() => {
    setOpen(false);
    setRaw("");
    setActive(0);
  }, []);

  /* Esc로 닫으면 **열기 전에 보던 곳으로** 포커스를 돌려놓습니다.
     복원은 `restoreRef`를 비우면서 **정확히 한 번**만 일어납니다. */
  useEffect(() => {
    if (open) {
      restoreRef.current = document.activeElement as HTMLElement | null;
      /* 렌더 뒤에 입력이 존재하므로 다음 프레임에 포커스를 줍니다. */
      const timer = window.setTimeout(() => inputRef.current?.focus(), 0);
      return () => window.clearTimeout(timer);
    }
    const previous = restoreRef.current;
    restoreRef.current = null;
    if (previous && document.contains(previous)) previous.focus();
    return undefined;
  }, [open]);

  /* 전역 단축키. 조합키라 입력 중에도 안전하지만, IME 조합 중과 다른 모달 위에서는
     열지 않습니다 — 조합 문자를 끊거나 모달을 겹치게 만들기 때문입니다.
     `modalOpen()`은 `alertdialog`(ConfirmDialog)도 봅니다. */
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "k" && event.key !== "K") return;
      if (!(event.metaKey || event.ctrlKey) || event.altKey) return;
      if (event.isComposing) return;
      if (!open && modalOpen()) return;
      event.preventDefault();
      setOpen((prev) => !prev);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [open]);

  /**
   * 열려 있는 동안의 모달 키보드 규약.
   *
   * - **Esc는 포커스가 어디에 있든 닫습니다.** 입력에만 붙이면 스크린리더나
   *   마우스로 포커스가 밖으로 나간 사용자가 팔레트에 갇힙니다.
   * - **Tab은 다이얼로그 안에서 돕니다.** 막지 않으면 `aria-modal="true"`라고
   *   말해 놓고 실제로는 뒤 화면으로 빠져나가, 보조기술이 없는 것으로 취급한
   *   배경 위젯에 포커스가 갑니다.
   * - IME 조합 중에는 둘 다 가로채지 않습니다 — Esc는 조합 취소가 우선입니다.
   */
  useEffect(() => {
    if (!open) return undefined;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.isComposing) return;
      if (event.key === "Escape") {
        event.preventDefault();
        close();
        return;
      }
      if (event.key !== "Tab") return;
      const root = dialogRef.current;
      if (!root) return;
      event.preventDefault();
      const items = focusablesIn(root);
      if (items.length === 0) {
        root.focus();
        return;
      }
      const at = items.indexOf(document.activeElement as HTMLElement);
      const next = event.shiftKey
        ? items[(at <= 0 ? items.length : at) - 1]
        : items[at < 0 ? 0 : (at + 1) % items.length];
      next?.focus();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [open, close]);

  const go = useCallback(
    (row: Row) => {
      if (row.kind === "route") {
        navigate({ pathname: row.route.path, search: navSearch });
        close();
        return;
      }
      const item = row.item;
      const params = new URLSearchParams(navSearch);
      params.set("res", `${item.group}/${item.version}/${item.resource}`);
      if (item.namespace) params.set("ns", item.namespace);
      else params.delete("ns");
      params.set("item", `${item.namespace ?? ""}/${item.name}/${item.uid}`);
      /* 고른 항목은 최근 목록의 맨 앞으로 올라갑니다(있으면 갱신). 저장소만
         바꾸고 이번 열기의 질의 키는 두므로 닫히는 길에 요청이 하나 더 나가지 않습니다. */
      rememberRecent({
        clusterId,
        group: item.group,
        version: item.version,
        resource: item.resource,
        namespace: item.namespace ?? "",
        name: item.name,
        uid: item.uid,
      });
      navigate({ pathname: "/resources", search: `?${params.toString()}` });
      close();
    },
    [close, clusterId, navSearch, navigate],
  );

  const onInputKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    /* IME 조합 중의 Enter는 글자를 확정하는 키입니다. 여기서 행을 실행하면
       한글을 치다가 화면이 바뀝니다. Esc·Tab은 window 핸들러가 맡습니다. */
    if (event.nativeEvent.isComposing) return;
    if (event.key === "ArrowDown") {
      event.preventDefault();
      if (rows.length > 0) setActive((i) => (i + 1) % rows.length);
      return;
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      if (rows.length > 0) setActive((i) => (i - 1 + rows.length) % rows.length);
      return;
    }
    if (event.key === "Home") {
      event.preventDefault();
      setActive(0);
      return;
    }
    if (event.key === "End") {
      event.preventDefault();
      setActive(Math.max(0, rows.length - 1));
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      const row = rows[active];
      if (row) go(row);
    }
  };

  if (!open) return null;

  const listId = `${baseId}-list`;
  const inputId = `${baseId}-input`;
  const activeRow = rows[active];
  const activeId = activeRow ? `${baseId}-${activeRow.id}` : undefined;

  const state = searchStateOf({
    enabled: canExplore,
    normalized,
    fresh,
    loading: search.isLoading,
    error: searchError,
    count: resourceRows.length,
    settled: search.isSuccess,
  });
  const recentState = recentStateOf({
    enabled: canExplore,
    fetching: recent.isFetching,
    error: recent.error,
    settled: recent.isSuccess,
    count: resourceRows.length,
  });
  /* degraded는 사유가 없어도 알립니다. 사유가 선택 필드라는 이유로 침묵하면
     "완전한 결과"와 구분되지 않습니다. */
  const degraded = Boolean(searchData?.degraded);
  const degradedReason = degraded ? (searchData?.reason ?? "").trim() : "";
  const truncated = Boolean(searchData?.truncated);

  return (
    <div className="palette__scrim" onMouseDown={close} data-testid="palette-scrim">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="명령 팔레트"
        className="palette"
        tabIndex={-1}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <label className="palette__field" htmlFor={inputId}>
          <span className="visually-hidden">화면 이동 또는 리소스 검색</span>
          <input
            id={inputId}
            ref={inputRef}
            className="palette__input"
            type="text"
            role="combobox"
            autoComplete="off"
            spellCheck={false}
            maxLength={SEARCH_MAX_QUERY}
            aria-expanded="true"
            aria-controls={listId}
            aria-autocomplete="list"
            {...(activeId ? { "aria-activedescendant": activeId } : {})}
            placeholder="화면 이동 또는 리소스 검색"
            value={raw}
            onChange={(e) => setRaw(e.target.value)}
            onKeyDown={onInputKeyDown}
          />
        </label>

        {/* 결과 수는 스크린리더에도 전달합니다 — 목록이 길어지면 시각 사용자만
            "몇 건인지"를 알 수 있게 두면 안 됩니다. */}
        <p className="palette__count" role="status" aria-live="polite">
          {rows.length}건의 결과
        </p>

        <ul id={listId} role="listbox" aria-label="결과" className="palette__list">
          {routeRows.length > 0 && (
            <li role="presentation" className="palette__group">
              이동
            </li>
          )}
          {routeRows.map((row) => {
            const index = rows.indexOf(row);
            return (
              <li
                key={row.id}
                id={`${baseId}-${row.id}`}
                role="option"
                aria-selected={index === active}
                className={index === active ? "palette__option is-active" : "palette__option"}
                onMouseDown={(e) => {
                  e.preventDefault();
                  go(row);
                }}
              >
                <span className="palette__primary">{row.route.label}</span>
                <span className="palette__meta">{row.route.path}</span>
              </li>
            );
          })}

          {canExplore && (
            <li role="presentation" className="palette__group">
              {isRecentMode ? "최근 항목" : "리소스 찾기"}
            </li>
          )}
          {resourceRows.map((row) => {
            const index = rows.indexOf(row);
            const item = row.item;
            return (
              <li
                key={row.id}
                id={`${baseId}-${row.id}`}
                role="option"
                aria-selected={index === active}
                className={index === active ? "palette__option is-active" : "palette__option"}
                onMouseDown={(e) => {
                  e.preventDefault();
                  go(row);
                }}
              >
                <span className="palette__primary">{item.name}</span>
                <span className="palette__meta">
                  {item.kind}
                  {item.namespace ? ` · ${item.namespace}` : " · 클러스터 범위"}
                  {!row.recent && MATCH_LABEL[item.matchedField] ? ` · ${MATCH_LABEL[item.matchedField]} 일치` : ""}
                </span>
              </li>
            );
          })}
        </ul>

        {/* 상태는 서로 다른 문구로 구분합니다. 같은 빈 화면으로 접지 않습니다.
            입력이 비어 있을 때는 **최근 항목의** 사유를 말합니다 — 검색 상태를
            그대로 재사용하면 "최근이 비었다"와 "최근을 못 불러왔다"가 섞입니다. */}
        {isRecentMode && recentState === "unavailable" && (
          <p className="palette__note" data-state="unavailable">
            이 배포에서는 리소스 검색을 사용할 수 없습니다. 화면 이동은 계속 쓸 수 있습니다.
          </p>
        )}
        {isRecentMode && recentState === "loading" && (
          <p className="palette__note" data-state="recent-loading" aria-busy="true">
            최근 항목을 다시 확인하는 중…
          </p>
        )}
        {isRecentMode && recentState === "empty" && (
          <p className="palette__note" data-state="recent-empty">
            최근 본 리소스가 없습니다. 이름을 입력하면 클러스터 전체에서 찾습니다.
          </p>
        )}
        {isRecentMode && recentState === "error" && (
          <p className="palette__note" data-state="recent-error" role="alert">
            최근 항목을 불러오지 못했습니다.
            {recent.error instanceof HttpError ? ` ${recent.error.body.message}` : ""}
          </p>
        )}
        {!isRecentMode && state === "unavailable" && (
          <p className="palette__note" data-state="unavailable">
            이 배포에서는 리소스 검색을 사용할 수 없습니다. 화면 이동은 계속 쓸 수 있습니다.
          </p>
        )}
        {state === "short" && (
          <p className="palette__note" data-state="short">
            리소스 검색은 {SEARCH_MIN_QUERY}자 이상 입력해야 시작됩니다.
          </p>
        )}
        {state === "loading" && (
          <p className="palette__note" data-state="loading" aria-busy="true">
            찾는 중…
          </p>
        )}
        {state === "syncing" && (
          <p className="palette__note" data-state="syncing">
            리소스 색인을 동기화하는 중입니다. 잠시 뒤 다시 시도하세요.
          </p>
        )}
        {state === "empty" && (
          <p className="palette__note" data-state="empty">
            일치하는 리소스가 없습니다. 권한 밖 리소스는 결과에 나오지 않습니다.
          </p>
        )}
        {state === "error" && (
          <p className="palette__note" data-state="error" role="alert">
            리소스를 검색하지 못했습니다.
            {searchError instanceof HttpError ? ` ${searchError.body.message}` : ""}
          </p>
        )}
        {/* 결과가 0건이어도 degraded는 함께 보여줍니다. "없음"과 "못 찾았음"을
            같은 문장으로 접으면 사용자가 리소스가 없다고 오판합니다.
            사유(reason)는 선택 필드라 없을 수 있고, 없다고 침묵하지 않습니다. */}
        {(state === "ready" || state === "empty") && degraded && (
          <p className="palette__note" data-state="degraded" role="note">
            색인이 완전하지 않습니다
            {degradedReason ? ` — ${degradedReason}` : ""}. 결과가 일부 빠졌을 수 있습니다.
          </p>
        )}
        {state === "ready" && truncated && (
          <p className="palette__note" data-state="truncated" role="note">
            상위 {SEARCH_MAX_RESULTS}건만 표시했습니다. 더 긴 접두사로 좁히세요.
          </p>
        )}

        <p className="palette__hint muted">
          ↑↓ 이동 · Enter 열기 · Esc 닫기
          {location.pathname === "/resources" ? " · / 이름 검색" : ""}
        </p>
      </div>
    </div>
  );
}
