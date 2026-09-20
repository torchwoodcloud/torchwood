import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { getProjectID, PROJECT_STORAGE_KEY, setProjectID } from "@/api/client";
import {
  login as apiLogin,
  logout as apiLogout,
  refreshSession,
} from "@/api/auth";

interface AuthContextValue {
  projectId: string | null;
  isAuthenticated: boolean;
  // loading 为 true 表示正在用 refresh cookie 探测初始会话状态。
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  selectProject: (projectID: string) => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  // 会话凭证在 HttpOnly cookie 里，JS 读不到；加载时用一次 refresh 探测
  // （成功即已登录并顺带续期），失败视为匿名。
  const queryClient = useQueryClient();
  const [authenticated, setAuthenticated] = useState(false);
  const [loading, setLoading] = useState(true);
  const [projectId, setProjectIdState] = useState<string | null>(() => getProjectID());

  useEffect(() => {
    let cancelled = false;
    refreshSession()
      .then(() => {
        if (!cancelled) {
          setAuthenticated(true);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setAuthenticated(false);
        }
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // 跨标签同步项目作用域：请求拦截器每次实时读 localStorage 注入
  // X-Torchwood-Project，而本标签的 React 状态只在挂载时读一次——标签 A
  // 切项目/登出后，标签 B 会"显示旧项目、写请求却带新项目 header"。订阅
  // storage 事件（仅其它标签的写入会触发）重新读取，让展示与实际作用域
  // 一致：A 切项目 → B 跟随；A 登出清项目 key → B 状态复位。key 为 null
  // 表示整体 clear()，同样按重读处理；本标签自身的写入不触发 storage
  // 事件，selectProject/logout 原路径不受影响。
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key !== null && event.key !== PROJECT_STORAGE_KEY) {
        return;
      }
      setProjectIdState(getProjectID());
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const login = useCallback(async (email: string, password: string) => {
    await apiLogin({ email, password });
    setAuthenticated(true);
  }, []);

  const logout = useCallback(async () => {
    // await 登出请求：失败说明会话可能仍有效（服务端未撤销），给出明确引导；
    // 无论成败都清理本地状态与全部查询缓存。
    try {
      await apiLogout();
    } catch {
      toast.error("登出失败，会话可能仍有效，请重试");
    }
    setProjectID(null);
    setProjectIdState(null);
    setAuthenticated(false);
    queryClient.clear();
  }, [queryClient]);

  const selectProject = useCallback((projectID: string) => {
    setProjectID(projectID);
    setProjectIdState(projectID);
  }, []);

  const value = useMemo(
    () => ({
      projectId,
      isAuthenticated: authenticated,
      loading,
      login,
      logout,
      selectProject,
    }),
    [projectId, authenticated, loading, login, logout, selectProject]
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within AuthProvider");
  }
  return ctx;
}
