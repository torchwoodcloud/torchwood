import type { CommandResult } from "@lynx-go/uikit-core/kernel";
import { updateCurrentAdmin, type Admin } from "@/api/admins";
import { skipToast as SKIP } from "@/api/client";
import { runCommandMapped } from "@/kit/errors";

// account 域命令族切片（§12.3，apiKeys 同款）：profile/preferences 自助写
// （PATCH /console/admins/me）。account 是单例 me——无集合语义，命令用裸
// CommandDescriptor（不挂 resource，无 kit 键可失效）；成功后由页面按现页
// 行为刷新 ["console-admin-me"] 缓存（setQueryData / 消费组件随重渲染取新
// 值）。结果两分映射——400→rejected（改密校验失败/弱密码）、409→conflict、
// 其余 throw ApiError；命令调用一律 __skipToast（toast 单源 = onSettled）。

// 改密：成功后服务端撤销全部凭证——页面负责强制回登录页（onSettled）。
export const changeCurrentPasswordCommand = {
  run: async (input: {
    current_password: string;
    new_password: string;
  }): Promise<CommandResult<Admin>> =>
    runCommandMapped(() =>
      updateCurrentAdmin(
        { current_password: input.current_password, new_password: input.new_password },
        SKIP,
      ),
    ),
};

// 时区偏好："" = 清除偏好（回退浏览器时区）；非空 = IANA 名。
export const updateCurrentTimezoneCommand = {
  run: async (input: { timezone: string }): Promise<CommandResult<Admin>> =>
    runCommandMapped(() => updateCurrentAdmin({ timezone: input.timezone }, SKIP)),
};
