// eslint flat config（ESLint 9+ / 10）
// 覆盖：src（renderer）+ electron（主进程/preload）+ 配置文件。
// 规则基调：typescript-eslint recommended + react-hooks 经典规则（rules-of-hooks /
// exhaustive-deps）。react-hooks 7 的 compiler 规则（immutability/static-components
// 等）对既有代码过严，暂关闭——作为增量目标逐步开启。
import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'

export default tseslint.config(
  { ignores: ['dist', 'dist-electron', 'release', 'node_modules', 'e2e', 'scripts', '.playwright-cli'] },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: 'module',
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      // react-hooks 经典规则（稳定、社区广泛采用）
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      // 全角空格豁免：UI 缩进有意用 \u3000（ApprovalCard 等排版）
      'no-irregular-whitespace': 'off',
      // JSX 内三元渲染表达式被误报（React 惯用法 {cond ? <A/> : <B/>}）
      '@typescript-eslint/no-unused-expressions': 'off',
      // prefer-const 对「let 声明 + 后续赋值」误报（timer = setTimeout 模式），关闭
      'prefer-const': 'off',
      // compiler 风格规则：既有代码大量模式不满足，关闭（增量目标）
      'react-hooks/immutability': 'off',
      'react-hooks/static-components': 'off',
      'react-hooks/use-memo': 'off',
      'react-hooks/preserve-manual-memoization': 'off',
      'react-hooks/incompatible-library': 'off',
      'react-hooks/set-state-in-effect': 'off',
      'react-hooks/refs-during-render': 'off',
      'react-hooks/components-compile-time-const': 'off',
      'react-hooks/no-direct-set-state-in-use-effect': 'off',
      'react-hooks/no-multi-reducer': 'off',
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
      // 项目现状：allow 常见宽松点（增量收紧）
      '@typescript-eslint/no-explicit-any': 'off', // 代码库大量 any（协议 map[string]any 对齐）
      '@typescript-eslint/no-unused-vars': ['warn', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      '@typescript-eslint/no-non-null-assertion': 'off', // 代码库惯用 !（DOM 挂载点等）
    },
  },
  {
    // 主进程/preload：Node 环境 + 独立 tsconfig（electron 文件不在 src tsconfig 的 projectService 内）
    files: ['electron/**/*.ts'],
    languageOptions: {
      globals: {
        process: 'readonly',
        __dirname: 'readonly',
        console: 'readonly',
        Buffer: 'readonly',
        setTimeout: 'readonly',
        clearTimeout: 'readonly',
      },
      parserOptions: {
        projectService: false, // electron 文件用 tsconfig.electron.json，不参与 src 项目服务
      },
    },
    rules: {
      // electron 是 CommonJS 产物（tsconfig.electron.json module=CommonJS），require 合法
      '@typescript-eslint/no-require-imports': 'off',
    },
  },
)
