# memgov 本地 Web

React + TypeScript + Vite 前端，复用 relayer-next 的记忆卡面和正文优先的详情阅读布局。任务、记忆、运行、设置仍接 memgov 的 `/api/v1` 接口，不引入套牌、角色或战斗数据模型。

开发需要 Node 22.16+ 和 npm。先运行已有 memgov 服务，再在仓库根目录执行：

```bash
make web-dev
```

打开 `http://127.0.0.1:5178/`，修改 `web/src/` 后由 Vite 热更新。开发服务器仅监听本机，把 `/api/v1`（含 SSE）代理到 `127.0.0.1:8787`；只有本地开发地址的 Origin 会适配为上游地址，生产服务的 Host、Origin 和控制请求校验不变。8787 应使用当前实际配置及数据目录，开发页连接的是该实例。

```bash
make web-check   # 类型检查和前端测试
make build       # 前端构建，然后生成 Go 二进制
make check       # 前端及 Go 检查
```

`internal/console/static/` 是受版本管理的生成目录，可让仅使用 Go 的源码检出仍有可嵌入页面；不要手工编辑。修改前端后用 `make build` 重新生成；macOS 系统托管服务自动加载替换后的程序，前台服务使用侧栏重启；随后刷新浏览器加载新页面。最终用户仍只运行 memgov，无需 Node 或 Vite。

- `src/components/`：React 通用、卡片和阅读组件。
- `src/pages/`：四个页面。
- `src/types.ts`、`src/api.ts`：现有接口的展示类型与客户端。
- `src/shared/`：沿用已测试的 API、时间、Markdown 及平台函数。
- `src/adapters/`：原有 YAML 编辑与只读 SSE 控制器，在隔离 DOM 容器内适配；本次没有改写它们的业务行为。

relayer-next 复用部分的许可见 [LICENSE-relayer](LICENSE-relayer)，许可同时内嵌在生产资源中。管理台范围见[主设计](../docs/design/local-console-design.md)和[详细稿](../docs/design/local-console-design-detail.md)。
