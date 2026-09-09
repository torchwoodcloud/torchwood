import axios from "axios";
import type { AxiosRequestConfig, InternalAxiosRequestConfig } from "axios";
import { toast } from "sonner";

// 会话凭证由 TORCHWOOD_session_console / TORCHWOOD_console_refresh HttpOnly
// cookie 携带，同源 XHR 自动附带，前端不再读写 token；一次性清理
// localStorage 中迁移前残留的旧 token。
localStorage.removeItem("TORCHWOOD_console_token");
localStorage.removeItem("torchwood_console_refresh_token");

export const api = axios.create({
  baseURL: "/v1",
  headers: {
    "Content-Type": "application/json",
  },
});

// ApiRequestConfig carries internal flags understood by the response
// interceptor: __skipAuthRetry opts a request out of the 401 refresh flow
// (e.g. sign-out), __authRetried marks a request already retried once,
// __skipToast suppresses the global error toast (caller renders its own
// error UI, e.g. login page / bulk operations / share-link generation).
export interface ApiRequestConfig extends AxiosRequestConfig {
  __skipAuthRetry?: boolean;
  __authRetried?: boolean;
  __skipToast?: boolean;
}

// refreshAuthTokenSingleFlight refreshes the console session via the
// HttpOnly refresh cookie (empty body; the server reads the cookie).
// Concurrent callers share one in-flight request so a burst of 401s
// triggers exactly one refresh. Uses bare axios to bypass the interceptors
// (a 401 from refresh itself must not recurse).
let refreshPromise: Promise<void> | null = null;

export function refreshAuthTokenSingleFlight(): Promise<void> {
  if (!refreshPromise) {
    refreshPromise = axios
      .post("/v1/console/auth/refresh")
      .then(() => {
        authRedirecting = false;
      })
      .finally(() => {
        refreshPromise = null;
      });
  }
  return refreshPromise;
}

export function setProjectID(projectID: string | null) {
  if (projectID) {
    localStorage.setItem("TORCHWOOD_console_project", projectID);
  } else {
    localStorage.removeItem("TORCHWOOD_console_project");
  }
}

export function getProjectID(): string | null {
  return localStorage.getItem("TORCHWOOD_console_project");
}

api.interceptors.request.use((config) => {
  const projectID = getProjectID();
  if (projectID) {
    config.headers["X-Torchwood-Project"] = projectID;
  }
  return config;
});

let authRedirecting = false;

function forceReLogin() {
  if (authRedirecting) {
    return;
  }
  authRedirecting = true;
  toast.error("会话已过期，请重新登录");
  setProjectID(null);
  // expired 标记是弹跳断路器:Login 页看到它就不再"探测成功自动跳回 console",
  // 必须显式重新登录——否则一旦存在持续的 401 源,console↔login 会无限互跳。
  window.location.href = "/console/login?expired=1";
}

api.interceptors.response.use(
  (response) => response,
  async (error) => {
    if (axios.isCancel(error)) {
      return Promise.reject(error);
    }
    const status = error?.response?.status;
    const config = error?.config as
      | (InternalAxiosRequestConfig & ApiRequestConfig)
      | undefined;
    const message =
      error?.response?.data?.error?.message ||
      error?.message ||
      "请求失败";

    if (status === 401) {
      const isLoginRequest = config?.url === "/console/auth/sign-in";
      const isMissingProject = message.includes("missing project context");
      if (isLoginRequest) {
        if (!config?.__skipToast) {
          toast.error(message);
        }
      } else if (
        config &&
        !config.__skipAuthRetry &&
        !config.__authRetried &&
        !isMissingProject
      ) {
        // Access cookie expired: refresh the session once (single-flight)
        // and retry the original request; only give up if refresh fails.
        try {
          await refreshAuthTokenSingleFlight();
          config.__authRetried = true;
          return api.request(config);
        } catch {
          forceReLogin();
        }
      } else if (!isMissingProject) {
        forceReLogin();
      }
    } else if (status >= 400 && !config?.__skipToast) {
      toast.error(message);
    }
    return Promise.reject(error);
  }
);
