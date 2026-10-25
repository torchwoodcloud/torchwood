import { Outlet as TanOutlet } from "@tanstack/react-router";
import { OutletContext } from "./outlet-context";

// Outlet（带 context 形态）：布局向子路由传参的承载件——context 经
// outlet-context.ts 的 React context（§12.9-5）。无 context 时直通 TanStack。
export function Outlet(props: { context?: unknown } & Record<string, unknown>) {
  const { context, ...rest } = props;
  if (context === undefined) return <TanOutlet />;
  return (
    <OutletContext.Provider value={context}>
      <TanOutlet {...(Object.keys(rest).length > 0 ? rest : {})} />
    </OutletContext.Provider>
  );
}
