# idp-workspace

IDP TELOS 的企业工作入口（Workspace）：工作空间与成员、应用注册与安装、工作收件箱、工作上下文、动作契约与应用桥。设计与决策见 [`docs/adr/ADR-0001-workspace-design.md`](docs/adr/ADR-0001-workspace-design.md)。

状态：**M1–M3 完成并实测**（ADR 第 3、5–10 节）。Workspace 只持有它自己的东西——工作空间、应用发布与安装、收件箱事项（作为视图）、成员的标记与工作上下文、动作调用记录；审批、运行与任务的状态由 idp-eer、idp-host 与应用自己持有。

- **应用注册**：清单 `idp.workspace.app/v1`（H5 入口、桥能力、动作契约、配置 Schema），发布不可变（摘要 = 规范化 JSON 的 sha256），可撤销。
- **安装**：每个工作空间每个应用一份，配置按清单的 Schema 校验，`revision` + If-Match。
- **收件箱**：生产者（`ws-producer`）按 `(producer, externalRef)` 幂等放入、只有生产者能结束；成员按主体或工作空间角色看到，自己的已读 / 归档标记互不影响。
- **运行的网页来源**（D29）：`host.run` 的运行结束后，收件箱里它的事项（`run:<runId>`）列出这次运行读过的网页来源——编号、标题（纯文字）、主机名、读取时间、回答是否引用了它，地址只有 http / https 才是链接（新窗口、`rel="noopener noreferrer"`，完整地址可见）；回答里写了、却不在来源里的编号单独提示。来源取自 idp-host 事件流的 `idp.host/run.sources`（idp-host 契约 1.11.0，先升 HOST），Workspace 逐项再校验后存在事项的 `sources` / `unverifiedCitations`（可选字段，不在摘要文字里）；失败、取消或跟随中断的运行没有来源。
- **动作**：成员调用应用的动作，Workspace 校验成员资格、发布状态、契约角色、事项是否提供该动作、输入 Schema，再转给拥有决定的系统：`host.run`（idp-host 建 AgentRun）、`eer.decision`（idp-eer 对 T2 审批作决定）、`webhook`（HMAC 签名调用应用后端）；每次调用留记录，`Idempotency-Key` 幂等。
- **应用桥**：`packages/bridge`（TypeScript、零依赖）——壳端只接受应用入口源的消息、只执行清单申请的能力；应用端带超时与关联的请求；webhook 签名校验（与 Go 端同一测试向量）。
- **Web 壳**（`apps/web`，React）：工作空间、收件箱（SSE 实时推送）、发起（按动作的输入 Schema 生成表单，发起不依附事项的动作，ADR D19）、应用导航与沙箱 frame、管理员的管理页（工作空间与成员、发布 / 撤销 / 安装应用、审计）；镜像内置并由服务同源提供。`host.run` 的结果自动回到发起者的收件箱。与真实 idp-host 的联调：`HOST=1 bash deploy/e2e.sh`；浏览器冒烟：`BROWSER=1 bash deploy/e2e.sh`。
- **与 agent 对话**（D22）：壳的"对话"页——选一个已安装应用提供的 agent，消息的回答逐字流出，每个工具调用的参数与结果、等待确认的工具调用（成员在页面上确认或拒绝）、取消、回到旧对话；Workspace 用成员自己的令牌中继 idp-host 的运行事件、线程与决定（只触达成员从这个工作空间发起的运行与线程），idp-host 仍是它们的所有者。
- **已连接的服务**（D30）：配置 `targets.connections`（idp-mcp-bridge 的内部 API、令牌交换的受众、Workspace 的公网地址）后，成员在壳的"已连接的服务"页给自己连接第三方服务（GitHub、Notion 等）：每个服务一次，在对方的页面上用 OAuth 授权，令牌由连接桥保管，Workspace 与智能体都看不到。页面列出每个连接的状态（未连接、已连接、需要重新授权与原因），连接、重新授权、断开（先确认）；有桥的评审角色的运维另看到评审账号的状态与连接 / 断开。回调落在 Workspace 自己的 `/oauth/callback`：只有这位成员自己发起的、没用过、10 分钟内的授权才会交给桥，其余一律不调用桥。`/oauth/client/<id>.json` 公开托管连接的客户端元数据文档，且只有文档的 `client_id` 正是这个地址时才发。对话里工具调用因没授权失败时，显示"需要先连接 <服务>"和去自己的页面的按钮；成员授权后再问一次。没配置时没有页面、没有路由。设置、回调路径与桥那一侧要对应的配置见 [`deploy/kubernetes/README.md`](deploy/kubernetes/README.md)。
- **成员的文件**（D23）：成员上传文件（只有所有者能看、下载、删除；类型白名单、拒绝宏、校验内容），动作用 `format: idp-file` 收文件；`host.run` 为每个附件签读票据、再签一张交付用的写票据（idp-host 契约 1.8.0），idp-sandbox 在内部监听（`IDP_WORKSPACE_INTERNAL_LISTEN`）上凭票据取文件、交付做好的文件；对话页可附文件、下载交付物。文件内容缺省在 PostgreSQL，`IDP_WORKSPACE_FILE_STORE=s3` 时放进 S3 兼容的对象存储（RustFS 等；按 SHA-256 核对，D24）。
- **我的记忆**（D21）：配置 `targets.cmi` 后，成员在壳的"记忆"页查看并删除助手存进 idp-cmi 的对话记忆（`GET /v1/me/memory`、`DELETE /v1/me/memory/{id}`、`DELETE /v1/me/memory`），始终用成员本人的令牌（可令牌交换）调用 CMI；删除写审计，不含内容；未配置时 404 `NOT_CONFIGURED`，壳隐藏该页。
- **登录与令牌**（M3）：壳以 OIDC 授权码 + PKCE 登录；应用可申请 `identity.token`，由 Workspace 签发给应用后端的短期 ES256 令牌（`/.well-known/jwks.json`，`packages/bridge` 的 `verifyAppToken` 校验）；下游凭据可用 RFC 8693 令牌交换。真实 Keycloak 的端到端：`OIDC=1 BROWSER=1 bash deploy/e2e.sh`。
- 契约 `idp.telos.workspace` 1.4.0 在 [`api/contracts/`](api/contracts)（`go test ./internal/workspace -run TestContract`），接口见 [`api/openapi.yaml`](api/openapi.yaml)。

## 运行

```
make test               # gofmt、vet、单元与契约测试
make test-integration   # 真实 PostgreSQL（Docker）上的集成测试，假 idp-host / idp-eer / 应用后端
make test-web           # 前端（npm workspaces）：bridge 与 web 壳的类型检查和测试
bash deploy/e2e.sh      # 镜像（含 web 壳）+ PostgreSQL 的 compose 端到端；HOST=1 加真实 idp-host，BROWSER=1 加 Chromium 冒烟
npm run dev -w apps/web # 壳的开发服务器，API 代理到 127.0.0.1:8560
```

CI 与 `deploy/e2e.sh`、`make test-integration` 里的第三方镜像（PostgreSQL、RustFS、Keycloak）先由 `deploy/pull-image.sh` 显式拉取并重试（最多 5 次，间隔 15、30、60、120 秒；本机已有则跳过），因为公共 registry 对匿名拉取按来源地址限流，而 CI 的 runner 与许多项目共用地址。

`idp-workspace migrate`（owner 连接）→ `idp-workspace ensure-login idp_ws_svc` → `idp-workspace seed-tenant acme` → `idp-workspace serve`（环境变量见 `cmd/idp-workspace/main.go`）。可选配置文件 `IDP_WORKSPACE_CONFIG`：

```yaml
targets:
  host:                                                              # 令牌交换：成员的令牌换成发给 idp-host 的令牌
    endpoint: https://host.example
    auth: exchange
    exchange: {tokenUrl: https://idp.example/realms/idp/protocol/openid-connect/token, clientId: idp-workspace, clientSecretFile: /run/secrets/exchange, audience: idp-host}
  cmi:  {endpoint: https://cmi.example, auth: exchange, exchange: {tokenUrl: https://idp.example/realms/idp/protocol/openid-connect/token, clientId: idp-workspace, clientSecretFile: /run/secrets/exchange, audience: idp-cmi}}  # 可选：我的记忆（D21）
  connections:                                                        # 可选：已连接的服务（D30）；auth 只能是 exchange
    endpoint: http://idp-mcp-bridge.idp-mcp-bridge.svc.cluster.local:8681
    auth: exchange
    exchange: {tokenUrl: https://idp.example/realms/idp/protocol/openid-connect/token, clientId: idp-workspace, clientSecretFile: /run/secrets/exchange, audience: idp-mcp-bridge}
    publicUrl: https://workspace.example                              # 只有协议与主机；桥的 IDP_BRIDGE_REDIRECT_URI = <publicUrl>/oauth/callback
  eer:  {endpoint: https://eer.example, auth: static, tokenFile: /run/secrets/eer, tenantId: eer-acme}
webhooks: {secretFile: /run/secrets/webhook}                         # 至少 32 字节
```

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
