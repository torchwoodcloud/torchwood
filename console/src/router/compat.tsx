import {
  Link as TanLink,
  Navigate as TanNavigate,
  Outlet as TanOutlet,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  useLocation as useTanLocation,
  useNavigate as useTanNavigate,
  useRouterState,
  useSearch,
} from "@tanstack/react-router";
import { RouterProvider } from "@tanstack/react-router";
import {
  createContext,
  useContext,
  useMemo,
  type ReactNode,
  type MouseEvent,
} from "react";

// react-router-dom → TanStack Router 过渡兼容层（design.md §12.2 裁决 1）。
// 页面文件只改 import 行（react-router-dom → @/router/compat），行为经本层
// 薄映射到 TanStack；后续批次随各页 fleet-ui 化逐页退 compat（页内改
// Route.useParams/useSearch 原生形态），本层清空即退役。
//
// 映射面 = 取证清单：Link(17)/useParams(16)/useNavigate(12)/useLocation(5)/
// Outlet(4)/NavLink(4)/useSearchParams(3)/Navigate(2)/useOutletContext(1)。

// ---- Outlet context（react-router 的 <Outlet context> 形态——TanStack 走
// route context，不透传 props；compat 用 React context 手工复刻同语义）----

const OutletContext = createContext<unknown>(null);

export function Outlet(props: { context?: unknown } & Record<string, unknown>) {
  const { context, ...rest } = props;
  if (context === undefined) return <TanOutlet />;
  return (
    <OutletContext.Provider value={context}>
      <TanOutlet {...(Object.keys(rest).length > 0 ? rest : {})} />
    </OutletContext.Provider>
  );
}

export function useOutletContext<T = unknown>(): T {
  return useContext(OutletContext) as T;
}

// ---- Link / NavLink ----

// className 两形态：静态串或 render-fn（{isActive}——Layout 侧栏用法）。
export interface CompatLinkProps {
  to: string;
  children?: ReactNode;
  className?: string | ((state: { isActive: boolean }) => string);
  title?: string;
  replace?: boolean;
  onClick?: (event: MouseEvent<HTMLAnchorElement>) => void;
  [key: string]: unknown;
}

export function Link({ to, children, className, ...rest }: CompatLinkProps) {
  const resolved = typeof className === "function" ? undefined : className;
  return (
    <TanLink to={to as never} className={resolved} {...(rest as object)}>
      {children}
    </TanLink>
  );
}

// NavLink 复刻 react-router 的 end 语义：end = 精确匹配；否则前缀匹配
// （pathname === to 或以 to + '/' 开头）。className 支持 render-fn
// （{isActive}）与静态串两形态（Layout 侧栏是 render-fn 用法）。
export function NavLink({
  to,
  end,
  className,
  children,
  ...rest
}: CompatLinkProps & { end?: boolean }) {
  const pathname = useTanLocation().pathname;
  const active = end
    ? pathname === to
    : pathname === to || pathname.startsWith(`${to}/`);
  const resolved =
    typeof className === "function"
      ? (className as (state: { isActive: boolean }) => string)({ isActive: active })
      : className;
  return (
    <TanLink
      to={to as never}
      className={resolved}
      data-status={active ? "active" : undefined}
      {...(rest as object)}
    >
      {children}
    </TanLink>
  );
}

// ---- hooks ----

// useParams：兼容 react-router 的泛型调用面（useParams<{id:string}>）——
// 经 useRouterState 取当前 match 链的合并参数（useParams({strict:false}) 的
// 等价面，绕开该版本 options 类型收窄）。
export function useParams<T = Record<string, string | undefined>>(): T {
  // select 返回合并对象违反 TanStack 结构共享约束——调用面宽松化
  // （运行时 = Object.assign 的 match 链参数合并，每次 location 变化重算）。
  const mergedParams = (useRouterState as unknown as (options: unknown) => Record<string, unknown>)({
    structuralSharing: false,
    select: (state: { matches: Array<{ params: Record<string, unknown> }> }) =>
      Object.assign({}, ...state.matches.map((match) => match.params)),
  });
  return mergedParams as T;
}

// useNavigate：字符串目标（含模板串）+ {replace}；torchwood 无 navigate(-1) 用法。
export function useNavigate() {
  const navigate = useTanNavigate();
  return (to: string, options?: { replace?: boolean }) => {
    void navigate({
      to,
      ...(options?.replace !== undefined ? { replace: options.replace } : {}),
    });
  };
}

// useLocation：pathname/search/hash——search 在 TanStack 是对象，这里复刻
// react-router 的查询串形态（消费方只读 pathname 的用法不受影响）。
function toQueryString(search: Record<string, unknown>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(search)) {
    if (value === undefined || value === null) continue;
    params.set(key, typeof value === "string" ? value : JSON.stringify(value));
  }
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}

function toPlainRecord(search: Record<string, unknown>): URLSearchParams {
  const out = new URLSearchParams();
  for (const [key, value] of Object.entries(search)) {
    if (value === undefined || value === null || value === "") continue;
    out.set(key, typeof value === "string" ? value : JSON.stringify(value));
  }
  return out;
}

export function useLocation(): { pathname: string; search: string; hash: string } {
  const location = useTanLocation();
  const search = useMemo(() => toQueryString(location.search as Record<string, unknown>), [
    location.search,
  ]);
  return { pathname: location.pathname, search, hash: location.hash };
}

// useSearchParams：[URLSearchParams, setter]——读经 strict:false 合并 search
// 构造；写经 navigate({to:'.', search: prev => next, replace})（函数式，
// 未提及的参数保留）。setter 兼容三入参形态：URLSearchParams / 对象字面量 /
// 函数（react-router 均接受）。缺省 replace 与 react-router 对齐 = push。
type SearchParamsInput =
  | URLSearchParams
  | Record<string, string>
  | ((prev: URLSearchParams) => URLSearchParams | Record<string, string>);

export function useSearchParams(): readonly [
  URLSearchParams,
  (next: SearchParamsInput, options?: { replace?: boolean }) => void,
] {
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  const navigate = useTanNavigate();

  const params = useMemo(() => toPlainRecord(search), [search]);

  const setSearchParams = (next: SearchParamsInput, options?: { replace?: boolean }) => {
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => {
        const prevParams = toPlainRecord(prev);
        const nextParams =
          typeof next === "function" ? next(prevParams) : next;
        const normalized =
          nextParams instanceof URLSearchParams
            ? nextParams
            : new URLSearchParams(
                Object.entries(nextParams).filter(
                  ([, v]) => v !== "" && v !== undefined,
                ),
              );
        const out: Record<string, string> = {};
        normalized.forEach((value: string, key: string) => {
          out[key] = value;
        });
        return out;
      },
      replace: options?.replace ?? false,
    });
  };

  return [params, setSearchParams] as const;
}

// ---- 直通项 ----

// Navigate：宽松 to（模板串进 Navigate 的页面存在——settings 重定向等）。
export function Navigate({ to, replace }: { to: string; replace?: boolean }) {
  return <TanNavigate to={to as never} replace={replace} />;
}

// ---- 测试面 ----
// TestRouter：MemoryRouter+Routes+Route 声明式的 TanStack 替身——routes 的
// path 支持 ':param' 与 '$param' 双写（内部统一转 $ 形）；initialEntries
// 定位首地址。测试文件用本组件替换旧包装（hooks 经 compat 在其内正常工作）。

// 测试路径归一：':param' → '$param'（react-router 写法）；'/*' → '/$'
// （TanStack splat 语法）。允许两种方言混写。
function normalizeTestPath(path: string): string {
  return path.replace(/:([A-Za-z0-9_]+)/g, "$$$1").replace(/\/\*$/, "/$");
}

interface TestRouteSpec {
  path: string;
  element: ReactNode;
  /** 嵌套子路由（Layout/Outlet 形态测试）；path "/" = index */
  children?: ReadonlyArray<TestRouteSpec>;
}

export function TestRouter({
  routes,
  initialEntries = ["/"],
}: {
  routes: ReadonlyArray<TestRouteSpec>;
  initialEntries?: string[];
}) {
  const signature = `${initialEntries.join("|")};${routes.map((r) => r.path).join("|")}`;
  const router = useMemo(() => {
    const root = createRootRoute({});
    const children = routes.map((spec) => {
      const route = createRoute({
        getParentRoute: () => root,
        path: normalizeTestPath(spec.path),
        component: () => <>{spec.element}</>,
      });
      if (spec.children && spec.children.length > 0) {
        return route.addChildren(
          spec.children.map((child) =>
            createRoute({
              getParentRoute: () => route,
              path: normalizeTestPath(child.path),
              component: () => <>{child.element}</>,
            }),
          ),
        );
      }
      return route;
    });
    return createRouter({
      routeTree: root.addChildren(children),
      history: createMemoryHistory({ initialEntries }),
    });
    // 路由表/入口按签名缓存——element 闭包按首渲染快照（测试单次渲染语义）
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature]);

  return <RouterProvider router={router} />;
}

// useRouterState 供少数需要 router 内部状态的消费方（目前仅本层使用）；
// 导出以免页面直接 import @tanstack/react-router 深层 API。
export { useRouterState };
