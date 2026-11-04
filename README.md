# idp-workspace

IDP TELOS 的企业工作入口（Workspace）：工作空间与成员、应用注册与安装、工作收件箱、工作上下文、动作契约与应用桥。设计与决策见 [`docs/adr/ADR-0001-workspace-design.md`](docs/adr/ADR-0001-workspace-design.md)。

状态：**M1、M2 完成并实测**（ADR 第 3、5–7 节）。Workspace 只持有它自己的东西——工作空间、应用发布与安装、收件箱事项（作为视图）、成员的标记与工作上下文、动作调用记录；审批、运行与任务的状态由 idp-eer、idp-host 与应用自己持有。

- **应用注册**：清单 `idp.workspace.app/v1`（H5 入口、桥能力、动作契约、配置 Schema），发布不可变（摘要 = 规范化 JSON 的 sha256），可撤销。
- **安装**：每个工作空间每个应用一份，配置按清单的 Schema 校验，`revision` + If-Match。
- **收件箱**：生产者（`ws-producer`）按 `(producer, externalRef)` 幂等放入、只有生产者能结束；成员按主体或工作空间角色看到，自己的已读 / 归档标记互不影响。
- **动作**：成员调用应用的动作，Workspace 校验成员资格、发布状态、契约角色、事项是否提供该动作、输入 Schema，再转给拥有决定的系统：`host.run`（idp-host 建 AgentRun）、`eer.decision`（idp-eer 对 T2 审批作决定）、`webhook`（HMAC 签名调用应用后端）；每次调用留记录，`Idempotency-Key` 幂等。
- **应用桥**：`packages/bridge`（TypeScript、零依赖）——壳端只接受应用入口源的消息、只执行清单申请的能力；应用端带超时与关联的请求；webhook 签名校验（与 Go 端同一测试向量）。
- **Web 壳**（`apps/web`，React）：工作空间、收件箱（SSE 实时推送）、应用导航与沙箱 frame；镜像内置并由服务同源提供。`host.run` 的结果自动回到发起者的收件箱。与真实 idp-host 的联调：`HOST=1 bash deploy/e2e.sh`；浏览器冒烟：`BROWSER=1 bash deploy/e2e.sh`。
- 契约 `idp.telos.workspace` 1.0.0 在 [`api/contracts/`](api/contracts)（`go test ./internal/workspace -run TestContract`），接口见 [`api/openapi.yaml`](api/openapi.yaml)。

## 运行

```
make test               # gofmt、vet、单元与契约测试
make test-integration   # 真实 PostgreSQL（Docker）上的集成测试，假 idp-host / idp-eer / 应用后端
make test-web           # 前端（npm workspaces）：bridge 与 web 壳的类型检查和测试
bash deploy/e2e.sh      # 镜像（含 web 壳）+ PostgreSQL 的 compose 端到端；HOST=1 加真实 idp-host，BROWSER=1 加 Chromium 冒烟
npm run dev -w apps/web # 壳的开发服务器，API 代理到 127.0.0.1:8560
```

`idp-workspace migrate`（owner 连接）→ `idp-workspace ensure-login idp_ws_svc` → `idp-workspace seed-tenant acme` → `idp-workspace serve`（环境变量见 `cmd/idp-workspace/main.go`）。可选配置文件 `IDP_WORKSPACE_CONFIG`：

```yaml
targets:
  host: {endpoint: https://host.example, auth: forward}            # 转发调用者令牌（需含 idp-host 的 audience）
  eer:  {endpoint: https://eer.example, auth: static, tokenFile: /run/secrets/eer, tenantId: eer-acme}
webhooks: {secretFile: /run/secrets/webhook}                         # 至少 32 字节
```

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
