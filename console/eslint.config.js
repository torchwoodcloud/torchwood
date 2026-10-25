import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: globals.browser,
    },
    rules: {
      // shadcn/ui 组件惯例：variants 常量随组件文件导出
      'react-refresh/only-export-components': [
        'error',
        { allowConstantExport: true, allowExportNames: ['useAuth'] },
      ],
      // 受控 Dialog 表单初始化模式（open 时同步初始值）为仓库惯例，规则过新
      'react-hooks/set-state-in-effect': 'off',
      // 路由换轨后禁回 react-router-dom（design.md §12.2 裁决 1）——一切
      // 路由 API 经 @/router/compat 过渡层或 @tanstack/react-router 原生。
      'no-restricted-imports': [
        'error',
        {
          paths: [
            {
              name: 'react-router-dom',
              message: '路由已换轨 TanStack Router——用 @/router/compat（过渡层）或 @tanstack/react-router（原生）',
            },
          ],
        },
      ],
    },
  },
  {
    files: ['src/router/compat.tsx'],
    rules: {
      // 兼容层注释提及 react-router-dom 字样即可，无 import 面
      'no-restricted-imports': 'off',
    },
  },
  {
    // 路由定义文件（router.tsx）与 react-router 兼容层（compat.tsx）导出
    // hooks/路由表/组件混合——fast-refresh 只 export-components 规则不适用
    files: ['src/router.tsx', 'src/router/compat.tsx'],
    rules: {
      'react-refresh/only-export-components': 'off',
    },
  },
  {
    files: ['src/components/ui/**'],
    rules: {
      // shadcn/ui 组件同时导出组件与 variants（cva 结果），fast-refresh 规则不适用
      'react-refresh/only-export-components': 'off',
    },
  },
  {
    // 应用入口（main.tsx）：createRoot 直渲染 + 根级 scope 绑定组件
    //（FleetScopeBinding，§12.10-③）——无导出面，入口不参与热替换，规则不适用
    files: ['src/main.tsx'],
    rules: {
      'react-refresh/only-export-components': 'off',
    },
  },
])
