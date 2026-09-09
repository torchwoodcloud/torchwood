import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useAuth } from "@/hooks/useAuth";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { getSetupStatus, signUp } from "@/api/auth";

type SetupState = "loading" | "login" | "setup";

const schemaResourceIDPattern = "^[a-z][a-z0-9]{0,27}$";
const schemaResourceIDHint = "小写字母开头，仅小写字母与数字，最长 28。创建后不可改。";

export function Login() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [setupToken, setSetupToken] = useState("");
  const [projectId, setProjectId] = useState("");
  const [databaseId, setDatabaseId] = useState("app");
  const [setupTokenRequired, setSetupTokenRequired] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  // 未初始化时展示「初始化设置」表单，初始化完成后回到登录态。
  const [setupState, setSetupState] = useState<SetupState>("loading");
  // setup-status 探测失败时展示错误 + 重试按钮，同时退回登录表单不阻塞登录。
  const [setupProbeError, setSetupProbeError] = useState<string | null>(null);
  const { login, isAuthenticated } = useAuth();
  const navigate = useNavigate();
  // expired=1 由 API 层 401 强制跳转携带(见 api/client.ts forceReLogin)。
  // 此时不能凭探测成功的 isAuthenticated 自动跳回 console——那正是
  // console↔login 无限弹跳的根源;必须显式重新登录。
  const [searchParams] = useSearchParams();
  const sessionExpired = searchParams.get("expired") === "1";

  const probeSetup = useCallback(() => {
    setSetupProbeError(null);
    setSetupState("loading");
    let cancelled = false;
    getSetupStatus()
      .then((status) => {
        if (!cancelled) {
          setSetupState(status.needs_setup ? "setup" : "login");
          setSetupTokenRequired(status.setup_token_required);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setSetupState("login");
          setSetupProbeError("无法获取初始化状态，可重试或直接登录");
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (isAuthenticated && !sessionExpired) {
      navigate("/console", { replace: true });
      return;
    }
    return probeSetup();
  }, [isAuthenticated, sessionExpired, navigate, probeSetup]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      await login(email, password);
      // 显式进入 Console:带 expired 标记时 useEffect 的自动跳转被抑制。
      navigate("/console", { replace: true });
    } catch (err) {
      const msg = extractErrorMessage(err);
      setError(msg || "登录失败");
    } finally {
      setLoading(false);
    }
  };

  const handleSignUp = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    if (password !== confirmPassword) {
      setError("两次输入的密码不一致");
      return;
    }
    setLoading(true);
    try {
      await signUp({
        email,
        password,
        setup_token: setupTokenRequired ? setupToken : undefined,
        project_id: projectId,
        database_id: databaseId,
      });
      // 会话 cookie 已由服务端下发；再调 login 同步本地认证状态后进入 Console。
      // API Key 不在引导阶段生成，登录后到 API Keys 页面创建。
      await login(email, password);
      navigate("/console", { replace: true });
    } catch (err) {
      const msg = extractErrorMessage(err);
      setError(msg || "初始化失败");
    } finally {
      setLoading(false);
    }
  };

  const isSetup = setupState === "setup";

  return (
    <div className="flex min-h-screen items-center justify-center overflow-y-auto bg-muted/40 p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Torchwood Console</CardTitle>
          <CardDescription>
            {isSetup ? "首次部署：创建第一个管理员账户" : "使用管理员账户登录"}
          </CardDescription>
        </CardHeader>
        <form onSubmit={isSetup ? handleSignUp : handleSubmit}>
          <CardContent className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
            </div>
            {isSetup && (
              <div className="space-y-2">
                <Label htmlFor="confirm-password">确认密码</Label>
                <Input
                  id="confirm-password"
                  type="password"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  required
                />
              </div>
            )}
            {isSetup && (
              <div className="space-y-2">
                <Label htmlFor="project-id">Project ID</Label>
                <Input
                  id="project-id"
                  value={projectId}
                  onChange={(e) => setProjectId(e.target.value)}
                  required
                  pattern={schemaResourceIDPattern}
                  maxLength={28}
                  placeholder="shop"
                />
                <p className="text-xs text-muted-foreground">{schemaResourceIDHint}</p>
              </div>
            )}
            {isSetup && (
              <div className="space-y-2">
                <Label htmlFor="database-id">Database ID</Label>
                <Input
                  id="database-id"
                  value={databaseId}
                  onChange={(e) => setDatabaseId(e.target.value)}
                  required
                  pattern={schemaResourceIDPattern}
                  maxLength={28}
                  placeholder="app"
                />
                <p className="text-xs text-muted-foreground">
                  规则同上。缺省 app，将作为项目自动创建的第一业务库（可改为其它合法
                  id）。系统用户 / 文件 / 组不在此库，走 Users / Storage / Groups。
                </p>
              </div>
            )}
            {isSetup && setupTokenRequired && (
              <div className="space-y-2">
                <Label htmlFor="setup-token">Setup Token</Label>
                <Input
                  id="setup-token"
                  type="password"
                  value={setupToken}
                  onChange={(e) => setSetupToken(e.target.value)}
                  placeholder="TORCHWOOD_SECURITY_SETUP_TOKEN"
                  required
                />
              </div>
            )}
            {setupProbeError && (
              <div className="flex items-center justify-between gap-2 rounded-md bg-destructive/10 px-3 py-2">
                <p className="text-sm text-destructive">{setupProbeError}</p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={probeSetup}
                >
                  重试
                </Button>
              </div>
            )}
            {sessionExpired && !error && (
              <p className="text-sm text-muted-foreground">会话已过期，请重新登录</p>
            )}
            {error && <p className="text-sm text-destructive">{error}</p>}
            {isSetup && (
              <p className="text-xs text-muted-foreground">
                注册后创建指定项目与数据库，首个管理员获得 owner 角色。
                API Key 请登录后到 API Keys 页面生成。
              </p>
            )}
          </CardContent>
          <CardFooter>
            <Button type="submit" className="w-full" disabled={loading || setupState === "loading"}>
              {loading ? (isSetup ? "初始化中..." : "登录中...") : isSetup ? "创建管理员并初始化" : "登录"}
            </Button>
          </CardFooter>
        </form>
      </Card>
    </div>
  );
}

function extractErrorMessage(err: unknown): string | undefined {
  return (
    err as {
      response?: { data?: { error?: { message?: string } } };
    }
  )?.response?.data?.error?.message;
}
