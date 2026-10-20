# WarpDeck

一个容器，提供双栈 SOCKS5 和 IPv4/IPv6 网关。**WarpDeck 是自定义封装，`warpdeck` 不是 Cloudflare 官方命令。**

容器只处理送到它的流量，负责容器内的服务、配置与进程恢复。用户自行配置接入和宿主机路由、防火墙。项目不包含宿主机接入服务，不挂载 Docker socket，不使用宿主机网络命名空间。

## 目录

- [功能与边界](#功能与边界)
- [给部署 Agent 的操作约定](#给部署-agent-的操作约定)
- [拉取镜像和首次启动](#拉取镜像和首次启动)
- [镜像标签与自动发布](#镜像标签与自动发布)
- [JSON 是唯一业务配置入口](#json-是唯一业务配置入口)
- [命令](#命令)
- [切换与失败处理](#切换与失败处理)
- [接入与 IPv6](#接入与-ipv6)
- [仅本机与其他容器使用 SOCKS5](#仅本机与其他容器使用-socks5)
- [部署验收](#部署验收)
- [运行与构建](#运行与构建)
- [日常维护与升级](#日常维护与升级)
- [故障排查](#故障排查)
- [项目结构](#项目结构)
- [隔离验证](#隔离验证)

## 功能与边界

| 能力 | 行为 |
| --- | --- |
| 三种模式 | `proxy` 只提供 SOCKS5，`gateway` 提供 IPv4/IPv6 转发，`both` 同时提供两者 |
| IPv4 出站 | `egress.ipv4` 独立选择 `warp` 或 `native` |
| IPv6 出站 | `egress.ipv6` 独立选择 `warp` 或 `native`，不强制域名使用某个地址族 |
| 接入控制 | SOCKS 和网关分别配置来源白名单，默认允许全部 IPv4/IPv6 来源 |
| 配置管理 | JSON 决定业务行为；命令只保存修改，明确 `reload` 后才应用 |
| 持久化 | 注册身份和业务状态分别存入数据卷；重启恢复最后明确应用的配置 |
| 宿主机接入 | 用户自行决定路由、防火墙及客户端接入；启动容器不会接管整机网络 |

提供 GHCR 镜像，稳定部署建议使用 **`ghcr.io/poohyeban/warpdeck:0.0.1`** 并固定 digest，也可从源码构建。仅支持 **Linux amd64**，没有 ARM64 原生镜像。SOCKS 使用来源 IP 认证，不提供用户名/密码认证；如果多个客户端经过同一个 NAT，容器只能按它实际看到的来源地址区分。**仓库的通用 Compose 仍默认使用 `latest`，将 TCP 40000 发布到宿主机所有地址，默认 JSON 允许全部来源。** 首次启动前必须按所需接入范围调整；仅供本机或容器使用时，按[本机部署步骤](#仅本机与其他容器使用-socks5)限制入口。

## 给部署 Agent 的操作约定

本节适用于帮助用户部署、升级或排障的 agent。已有用户要求优先；不要重复询问已经明确的选择，也不要把某一台服务器的路径、端口或网络当作项目默认值。

1. **先检查现状。** 核对架构、Docker/Compose、TUN、已有容器、镜像、Compose 项目名和实际挂载。升级已有部署时读取该部署的 Compose 与已应用状态，保留目录、端口、来源列表、数据卷和注册身份；不要用仓库示例覆盖现有配置。
2. **确定业务需求。** 明确 `proxy` / `gateway` / `both`、两个地址族各自的出口、客户端位置、监听端口和允许来源。只需要 SOCKS5 时用 `proxy`；宿主机没有原生 IPv6 不代表应禁用 WARP IPv6。用户没有说明的关键接入需求才需要询问。
3. **分别配置两层。** Compose 管镜像、发布端口、网络、挂载和资源；JSON 管业务行为。保存 JSON 后执行 `reload` 或 `config apply`，再确认 `config_in_sync=true`。宿主机端口、容器端口和 JSON 端口不要混淆。
4. **遵守网络范围。** 容器只处理进入它的流量。不要为了部署代理添加宿主机策略路由、手工防火墙规则或改 DNS，也不要使用 host 网络、特权模式或 Docker socket。Docker 桥接和端口映射本身会创建宿主机规则；若用户连这些自动规则也禁止，先明确可用网络方案，不能声称普通桥接 Compose 完全不改宿主机规则。
5. **固定版本并保留恢复路径。** 按[升级步骤](#日常维护与升级)保存旧镜像引用和私有备份，核对新镜像版本、源码 revision 和 digest，只重建 `warp` 服务。已有注册不需要重新注册；`--accept-tos` 表示接受 Cloudflare 条款，不能把它当作普通健康检查。
6. **以实际请求验收。** 同时检查 Docker 健康、应用状态和每个启用地址族的真实请求，并从实际客户端位置验证接入。按用户约定观察时长；未指定时可用下方约 1 分钟示例。报告成功轮数、重启情况和检查范围，不把短时通过描述成长期稳定保证。成功后才按明确镜像引用清理旧资源，不全局 prune，不删除卷。

通过 `ssh ... 'bash -s'` 给远程 shell 输送脚本时，避免后续命令读取脚本输入：使用不带 `-i` 的 `docker exec`，或为 `docker compose exec -T ...` 加 `< /dev/null`。非交互 agent 命令使用 `-T`；不要分配不需要的终端。

## 拉取镜像和首次启动

支持 Linux amd64、Docker Engine 和 Docker Compose v2，需要 `/dev/net/tun`、容器内 NET_ADMIN，以及到 Cloudflare 注册服务和隧道端点的连通性。默认模式为 **proxy**，IPv4 和 IPv6 出站都选 **warp**，来源为 `0.0.0.0/0` 和 `::/0`。

**容器不检测宿主机是 IPv4-only、IPv6-only 还是双栈，也不据此替用户选择配置。** WARP 的承载网络与流量的目标地址族是两回事：只有原生 IPv4 的机器可以通过 WARP 访问 IPv6；只有原生 IPv6 的机器也可以通过 WARP 访问 IPv4。前提是隧道能够建立，容器内内核支持相应地址族。选择 `native` 则要求用户提供该地址族的原生路径；不可达时请求失败，不自动改用 WARP 或另一个地址族。

配置应用只验证 JSON 和容器本地配置，不把外网探测成功作为生效条件。`reload` 成功表示配置已应用，不保证隧道或任意网站此刻可达。

DNS 由用户的 Docker 部署提供，容器不会检测宿主机后替换 DNS。首次注册、隧道连接和域名解析需要可达的解析器；例如仅有 IPv6 承载的测试环境，应由部署者提供能经该网络访问的 DNS，而不能继承只有 IPv4 地址的解析器。可以在 Compose 服务下自行设置 `dns:`；示例值分别为 IPv4 `1.1.1.1`、IPv6 `2606:4700:4700::1111`，按自己的网络选择。`egress` 控制业务目标的出口，Docker 内置 DNS 的上游转发由部署环境负责。

宿主机不需要安装 Go、WARP、3proxy 或 Debian 软件包工具。直接部署需要访问 GHCR 下载镜像；运行阶段需要访问 Cloudflare 注册服务和隧道端点。公开镜像无需 GitHub 账户或 `docker login`。

获取部署文件并进入项目目录（克隆源码不代表需要编译）：

```sh
git clone https://github.com/poohyeban/warpdeck.git
cd warpdeck
```

如果不想克隆仓库，也可以只下载通用 Compose 文件：

```sh
mkdir warpdeck && cd warpdeck
curl -fL https://raw.githubusercontent.com/poohyeban/warpdeck/v0.0.1/compose.yaml -o compose.yaml
```

以上两种方式选一种即可。先按接入范围修改 Compose 的 `ports`（[本机示例](#仅本机与其他容器使用-socks5)），再选择镜像。以下写入命令只用于尚无 `.env` 的新目录；已有文件只修改 `WARPDECK_IMAGE` 一项，保留其他变量：

```sh
printf '%s\n' 'WARPDECK_IMAGE=ghcr.io/poohyeban/warpdeck:0.0.1' > .env
```

检查宿主机环境后拉取、启动：

```sh
docker version
docker compose version
test -c /dev/net/tun
```

如果 `/dev/net/tun` 不存在，需要由宿主机或 VPS 平台提供 TUN 支持；容器无法替代宿主机内核提供这个设备。


```sh
docker compose pull
docker compose up -d
# 首次空数据卷会等待注册；此时尚未健康是预期状态。
# 确认接受 Cloudflare 条款后，手动注册：
docker compose exec warp warpdeck register --accept-tos
docker compose exec warp warpdeck status
```

注册使用官方 `warp-cli registration new`，身份保存在独立的 `warp-data` 数据卷。已有身份会复用；重复执行注册命令不会更换身份，注册失败也不会删除已有数据。构建镜像时不注册，不包含任何机器的身份。

完成注册和所需来源限制后，验证两个目标地址族（若改过宿主机端口，相应替换 `40000`）：

```sh
curl --fail --max-time 20 --socks5-hostname 127.0.0.1:40000 https://1.1.1.1/cdn-cgi/trace
curl -g --fail --max-time 20 --socks5-hostname 127.0.0.1:40000 'https://[2606:4700:4700::1111]/cdn-cgi/trace'
```

默认策略下两者连通时均应看到 `warp=on`。SOCKS 连接自身可以通过 IPv4 或 IPv6 建立，与目标地址族独立。域名默认优先 IPv6，用户可通过 `socks.resolution` 改为 `prefer_ipv4`；这只改变解析偏好，不改变每个地址族的出站选择。

启动容器不会让宿主机整机流量自动进入容器。需要网关时，用户手动选择 `gateway` 或 `both`，并自行安排路由和接入。Compose 网段与现有网络冲突时由用户调整。
查看启动和应用结果可用 `docker compose logs --tail 80 warp`。官方守护进程的详细输出可能包含账户信息，因此默认不转发到 Docker 日志；连接诊断可用 `docker compose exec warp warp-cli --accept-tos status`。未注册时先完成上述命令；注册失败时检查网络连通性，再重试注册，勿删除正常的数据卷。注册完成后若隧道尚未连接，查看 `warpdeck status` 和官方客户端状态。需要保留数据时使用 `docker compose down`；加上 `-v` 会删除注册和业务配置。

官方说明：[WARP Linux 注册与连接](https://developers.cloudflare.com/warp-client/get-started/linux/)、[软件包来源](https://pkg.cloudflareclient.com/)。

## 镜像标签与自动发布

当前版本：[v0.0.1](https://github.com/poohyeban/warpdeck/releases/tag/v0.0.1)。修复隧道路由检测中的管道误判，详情见 [更新记录](CHANGELOG.md)。

镜像地址：[ghcr.io/poohyeban/warpdeck](https://github.com/poohyeban/warpdeck/pkgs/container/warpdeck)。构建记录：[GitHub Actions](https://github.com/poohyeban/warpdeck/actions/workflows/image.yml)。

| 引用 | 用途 |
| --- | --- |
| `ghcr.io/poohyeban/warpdeck:latest` | `main` 分支最近一次成功发布，通用 Compose 的回退默认值，可能先于稳定版本变化 |
| `ghcr.io/poohyeban/warpdeck:main` | 与主分支发布对应 |
| `ghcr.io/poohyeban/warpdeck:sha-<完整提交 SHA>` | 按源码提交查找镜像；同一提交重新构建时仍可能更新 |
| `ghcr.io/poohyeban/warpdeck:0.0.1` | 对应 `v0.0.1` 版本标签，通过发布工作流构建 |
| `ghcr.io/poohyeban/warpdeck:0.0.1@sha256:<摘要>` | 同时标明版本并固定镜像内容，建议用于稳定部署；摘要以实际拉取结果为准 |

通过环境变量选择已有标签或 digest。建议把选择写入本机 `.env`（已被 Git 忽略），之后每次 Compose 操作都会使用同一镜像：

```sh
# 先拉取目标版本，再检查其仓库摘要与源码版本
docker pull ghcr.io/poohyeban/warpdeck:0.0.1
docker image inspect ghcr.io/poohyeban/warpdeck:0.0.1 \
  --format 'digests={{json .RepoDigests}} version={{index .Config.Labels "org.opencontainers.image.version"}} revision={{index .Config.Labels "org.opencontainers.image.revision"}}'
```

把 `.env` 的 `WARPDECK_IMAGE` 改为 `ghcr.io/poohyeban/warpdeck:0.0.1@sha256:<RepoDigests 中的实际摘要>`，不要把示例占位符原样写入。`RepoDigests` 中仓库引用的摘要用于拉取固定内容，不能用容器 ID、源码 SHA 或随意选取的本地镜像 ID 替代。随后执行 `docker compose config --quiet` 和 `docker compose config --images` 核对解析结果；shell 中同名变量也可能覆盖 `.env`。

`latest` 和 `main` 是移动标签，版本标签也不能替代内容摘要；获取镜像的新版本需要手动 `pull` 并重建容器，运行中的容器不会自动更新。仅发布 `linux/amd64`；Apple Silicon 或 ARM VPS 不属于原生支持平台。

工作流在推送 `main`、推送 `v*` 标签或手动运行时构建；PR 只构建和检查，不登录 GHCR、不发布。手动运行限定在 `main` 或 `v*` 标签。构建包含路由管道回归测试、Go 单元测试、动态库检查，以及镜像命令、默认 JSON 与隔离命名空间内的规则语法校验；检查通过后才发布。Actions 不进行 WARP 注册，不包含注册身份，也不连接任何现有服务器。端到端 WARP 网络测试仍需在独立测试环境执行。

发布使用仓库自带的 `GITHUB_TOKEN`，不需要在仓库保存个人 PAT。第三方 Actions 固定到提交 SHA；镜像带源码链接和源码 revision 标签。首次创建 GHCR 包时，维护者需要在包设置中确认其可见性为 Public；公开仓库不会自动保证包也公开。参见 [GitHub 包可见性说明](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility)。Fork 项目会发布到 Fork 所有者的 GHCR 命名空间，需相应修改本机 `WARPDECK_IMAGE`；包首次公开设置也需自行完成。

## JSON 是唯一业务配置入口

配置位于 **`/var/lib/warpdeck/config.json`**，保存在 `warpdeck-state` 数据卷中。首次启动复制 [config.example.json](config.example.json)，默认仅开启 SOCKS，两个来源列表允许全部 IPv4/IPv6 来源。示例不含任何实际服务器的地址。

完整默认配置如下：

```json
{
  "version": 2,
  "mode": "proxy",
  "socks": {
    "listen": "::",
    "port": 1080,
    "allowed_sources": [
      "0.0.0.0/0",
      "::/0"
    ],
    "max_connections": 32,
    "resolution": "prefer_ipv6",
    "udp": true
  },
  "gateway": {
    "interface": "eth0",
    "allowed_sources": [
      "0.0.0.0/0",
      "::/0"
    ],
    "tcp_mss": 1220,
    "masquerade": {
      "ipv4": true,
      "ipv6": true
    }
  },
  "egress": {
    "ipv4": "warp",
    "ipv6": "warp"
  }
}
```

命令、直接编辑文件都操作同一份 JSON；没有 `MODE` 环境变量或独立模式文件覆盖它。编辑文件和保存命令不会自动应用。普通重启及保留数据卷的容器重建使用上次明确应用的配置，待确认修改不会因重启而生效。WARP 注册使用独立的 `/var/lib/cloudflare-warp` 数据卷。

| 字段 | 含义 |
| --- | --- |
| `version` | 配置版本，目前为 `2`；可读取旧版 `1` 配置 |
| `mode` | `proxy`、`gateway` 或 `both` |
| `egress.ipv4` / `egress.ipv6` | 各自独立选择 `warp` 或 `native`，作用于 SOCKS 和网关 |
| `socks.listen` | SOCKS 监听 IP，可设本地 IPv4/IPv6 地址或通配地址 |
| `socks.port` | SOCKS TCP 端口，1–65535 |
| `socks.allowed_sources` | 允许连接 SOCKS 的来源 IP/CIDR，支持 IPv4/IPv6；`[]` 拒绝全部 |
| `socks.max_connections` | 最大并发连接数，正整数，最大 2147483647；实际容量受资源和文件描述符限制 |
| `socks.resolution` | `prefer_ipv6` 或 `prefer_ipv4`，只决定域名解析优先顺序 |
| `socks.udp` | 是否启用 SOCKS5 UDP ASSOCIATE |
| `gateway.interface` | 容器内接收入站转发流量的接口，例如 `eth0` |
| `gateway.allowed_sources` | 允许网关转发的来源 IPv4/IPv6 IP 或 CIDR；`[]` 拒绝全部 |
| `gateway.masquerade.ipv4` / `.ipv6` | 分别启用或关闭该地址族的源地址伪装（默认均为 `true`） |
| `gateway.tcp_mss` | TCP MSS 上限，1–65535；`0` 关闭调整 |

所有字段必填。未知字段、重复 JSON 键、错误类型、无效地址和越界参数均会被拒绝。CIDR 会规范化并去重。允许任意有效来源范围，包括 `0.0.0.0/0` 和 `::/0`；`0.0.0.0` 本身表示单个地址，并不等于 `/0`。`[]` 表示拒绝全部来源。白名单匹配**容器实际看到的来源地址**，经过 NAT 后可能是宿主机或桥接网关地址。

旧版 `version: 1` 配置读取时转换为等价策略：IPv4 `warp`、IPv6 `native`、IPv4 masquerade 开启、IPv6 masquerade 关闭，保留原模式与来源列表。仅查看不会重写文件；保存或明确应用后使用 v2。新默认值不会覆盖已有数据卷。旧镜像无法读取 v2，降级需配套恢复升级前配置。

Docker 端口映射、容器地址、数据卷、权限、资源上限属于外部部署配置，仍由 Compose 管理；这份 JSON 不会修改宿主机。WARP 的注册身份仍由官方客户端数据卷管理。

## 命令

以下统一使用 Compose 服务名 `warp`，在项目目录执行；也可用 `docker exec <实际容器名> warpdeck ...`。

```sh
docker compose exec warp warpdeck status
docker compose exec warp warpdeck status --json
docker compose exec warp warpdeck config show
docker compose exec warp warpdeck config validate

# 这些命令只保存，不立即切换。
docker compose exec warp warpdeck mode proxy
docker compose exec warp warpdeck mode gateway
docker compose exec warp warpdeck mode both

# 来源可自由设置；以下示例允许全部地址族。
docker compose exec warp warpdeck config set socks.allowed_sources '["0.0.0.0/0","::/0"]'
docker compose exec warp warpdeck config set gateway.allowed_sources '["0.0.0.0/0","::/0"]'
docker compose exec warp warpdeck config set egress.ipv4 '"warp"'
docker compose exec warp warpdeck config set egress.ipv6 '"native"'
docker compose exec warp warpdeck config set socks.max_connections 32
docker compose exec warp warpdeck config set socks.udp false

# 查看、校验，然后明确应用。
docker compose exec warp warpdeck config show
docker compose exec warp warpdeck config validate
docker compose exec warp warpdeck reload
```

| 操作 | 保存请求配置 | 应用到运行服务 |
| --- | --- | --- |
| `mode` / `config set` | 是 | 否 |
| 直接编辑 `config.json` | 是 | 否 |
| `config validate` | 否 | 否 |
| `reload` | 使用当前请求配置 | 是 |
| `config apply FILE` | 校验后导入文件 | 是 |
| 容器重启或重建（保留卷） | 不改变待确认内容 | 恢复上次已应用快照 |

`mode both` 等价于把 JSON 的 `mode` 改为 `both`。`config set FIELD JSON_VALUE` 的值必须是 JSON，例如字符串需写成 `'"prefer_ipv6"'`。`mode` 和 `config set` 通过文件锁串行保存，校验后原子替换文件，明确提示尚未应用；执行 `reload` 才生效。失败返回非零状态。

要同时修改多个字段，可准备完整 JSON。`config apply FILE` 是另一条明确应用命令，会校验、导入并应用该文件：

```sh
docker compose cp ./config.json warp:/tmp/new-config.json
docker compose exec warp warpdeck config validate /tmp/new-config.json
docker compose exec warp warpdeck config apply /tmp/new-config.json
```

也可以直接编辑数据卷中的 `config.json`。**没有文件监听、定时配置检测或自动应用。** 建议先写临时文件，再在同一目录原子重命名。确认内容后执行以下命令，它会固定本次确认的内容、校验并等待应用结果：

```sh
docker compose exec warp warpdeck reload
```

如果使用宿主机绑定目录替代命名卷，应挂载**整个可写目录**到 `/var/lib/warpdeck`，不要只挂载单个 `config.json`；原子替换需要目录可写。

## 切换与失败处理

| 模式 | SOCKS5 | 容器 IPv4/IPv6 网关 |
| --- | --- | --- |
| `proxy` | 开启 | 关闭 |
| `gateway` | 关闭 | 开启 |
| `both` | 开启 | 开启 |

执行 `reload` 后，模式切换、网关白名单和 MSS 修改无需重启容器或 WARP 隧道。SOCKS 配置修改只重启 SOCKS 子进程；关闭或重启 SOCKS 会中断现有代理连接。关闭网关会停止转发。修改出站选择会重新配置 WARP，并重启 SOCKS，现有连接可能中断。`reload` / `config apply` 等待本地配置应用，不等待外网连通；连接失败不会改变用户选择。WARP 路径暂不可用时对应流量失败，不回退到原生出口。

- 保存命令校验 JSON 格式和参数；应用命令另外检查接口、监听地址与端口冲突，失败不切换服务。
- 手动编辑或保存命令不触发运行配置变化；`status` 显示请求内容与已应用状态是否一致。错误 JSON 不影响当前服务。
- 应用阶段失败时，尝试恢复上次运行配置；JSON 保留请求内容，状态明确显示未同步。修正 JSON 后重新应用。
- `/var/lib/warpdeck/applied.json` 是内部生成的已应用快照，不能作为用户配置编辑。重启、崩溃恢复和重建使用它，即使 `config.json` 有未确认或无效修改也不会自动启用。
- 首次启动没有快照时从 `config.json` 初始化（默认 SOCKS、双栈 WARP、允许全部来源）；首次配置无效或已有恢复快照损坏时启动失败。成功应用后更新快照。删除整个状态卷会同时清除配置与快照。

`status --json` 提供 `registration`、`requested_mode`、实际 `mode`、`config_in_sync`、`config_error`、SOCKS、网关和隧道状态。`ipv4_gateway=enabled` / `ipv6_gateway=enabled` 仅表示容器具备转发能力，不表示宿主机已经接入。内部已应用快照和状态文件只用于报告/恢复，不是额外用户配置入口。

下面是已注册、隧道连接且 `both` 已应用时的状态示意，不包含任何账户身份或出口 IP：

```json
{
  "name": "WarpDeck",
  "registration": "registered",
  "requested_mode": "both",
  "mode": "both",
  "config_path": "/var/lib/warpdeck/config.json",
  "config_in_sync": true,
  "config_error": "",
  "socks": "running",
  "ipv4_gateway": "enabled",
  "ipv6_gateway": "enabled",
  "tunnel": "connected",
  "egress": {"ipv4": "warp", "ipv6": "warp"},
  "routes": {"ipv4": "available", "ipv6": "available"}
}
```

`config_in_sync=false` 表示有待应用的修改或配置错误；先检查 `config_error` 和配置内容，再决定是否执行 `reload`。`egress` 描述已应用策略，`routes` 仅检查容器本地路由是否存在，不探测公网；连通性需由真实请求验证。

## 接入与 IPv6

通用 Compose 示例给容器分配 `172.28.80.2` 与 `fd80:28::2`。TCP 1080 发布为宿主机 TCP 40000，默认监听 `::`，配合容器 `net.ipv6.bindv6only=0` 接受 IPv4/IPv6。也可指定 `0.0.0.0` 或容器某个具体地址。JSON 修改监听端口不会自动修改 Docker 端口映射。

SOCKS5 支持 TCP CONNECT 和 UDP ASSOCIATE，均支持 IPv4/IPv6 目标。UDP 返回容器地址和动态 UDP 端口，客户端必须能路由到该地址与端口；仅发布 SOCKS TCP 端口不足以提供 UDP 接入。用户决定使用容器网络、路由或其他转发方式。

| IPv4 策略 | IPv6 策略 | 含义 |
| --- | --- | --- |
| `warp` | `warp` | 两个目标地址族都经过 WARP（默认） |
| `warp` | `native` | IPv4 经 WARP，IPv6 走原生路径 |
| `native` | `warp` | IPv4 走原生路径，IPv6 经 WARP |
| `native` | `native` | 两者都走原生路径，不要求 WARP 注册或连接 |

只需要原生代理或原生网关时，可以在首次启动、尚未注册 WARP 的状态下手动设置并应用：

```sh
docker compose exec warp warpdeck config set egress '{"ipv4":"native","ipv6":"native"}'
docker compose exec warp warpdeck reload
```

该表是用户可选策略，不是宿主机类型的自动匹配表。容器不会管理宿主机防火墙、DNS 或默认路由。容器内会维护自身策略路由、转发过滤与可选 masquerade，并抑制可能让客户端绕过本网关的 ICMP 重定向；这些均限于容器网络命名空间。Docker 自身负责用户所选桥接网络和端口发布的宿主机规则。请使用独立 Docker 网络，不使用 `network_mode: host`。

网关默认对两个地址族进行 masquerade。关闭某一项后保留相应客户端源地址，用户需要确保上游接受该源地址并具备正确的回程路由；关闭 NAT 不会自动获得可路由前缀。网关支持双栈转发，不提供 NAT64/NAT46，目标地址族不会被转换。

原生路由取自容器启动时已有网络。用户修改 Docker 网络连接或基础路由后应重建容器，使原生路由快照与部署一致。`reload` 仅应用业务 JSON，不重建 Docker 网络。

`prefer_ipv6` 使用 3proxy `-64`，`prefer_ipv4` 使用 `-46`；这控制域名解析优先级，明确指定 IP 时保留其地址族。不实现连接失败后的 Happy Eyeballs 竞速或跨地址族重试。地址族不可达时请求失败，用户可按自己的需要调整策略或解析偏好。

## 仅本机与其他容器使用 SOCKS5

以下是可选部署方案：宿主机通过 `127.0.0.1:40000` 使用代理，接入同一 Docker 网络的客户端通过 `warp:1080` 使用代理。它保持 `proxy` 模式，不接管宿主机或客户端容器的默认路由。客户端应用仍需显式配置 SOCKS5；接入网络本身不会自动代理流量。

**首次 `up` 前**直接修改部署用的 `compose.yaml`。在已有字段上替换下列值，其余设备、权限、sysctl、卷、资源限制和健康检查保持不变。下面仅是修改位置示意，不是完整 Compose：

```yaml
services:
  warp:
    ports:
      - "127.0.0.1:40000:1080/tcp"
networks:
  gateway:
    name: warpdeck_proxy
```

不要简单在另一份覆盖文件中追加本机 `ports`：[Compose 合并规则](https://docs.docker.com/reference/compose-file/merge/#unique-resources)可能保留原有公开映射。启动前用 `docker compose config` 确认只有预期的发布地址。先检查网段 `172.28.80.0/24`、`fd80:28::/64` 及网络名称无冲突；已有部署保留其实际网络名。该网络应专用于可信代理客户端。

按[首次启动](#拉取镜像和首次启动)完成启动和注册后，保存并应用本机方案。若改过桥接网段，以下白名单也要同步替换：

```sh
docker compose exec -T warp warpdeck mode proxy
docker compose exec -T warp warpdeck config set socks.listen '"::"'
docker compose exec -T warp warpdeck config set socks.port 1080
docker compose exec -T warp warpdeck config set socks.allowed_sources '["127.0.0.1/32","::1/128","172.28.80.0/24","fd80:28::/64"]'
docker compose exec -T warp warpdeck config set gateway.allowed_sources '[]'
docker compose exec -T warp warpdeck config set egress '{"ipv4":"warp","ipv6":"warp"}'
docker compose exec -T warp warpdeck config validate
docker compose exec -T warp warpdeck reload
docker compose exec -T warp warpdeck status --json
```

这里 `::` 是**容器内**的双栈监听地址；公开范围由宿主机发布地址和来源白名单共同限制。不要把容器内监听改成 `127.0.0.1` 来限制宿主机入口，否则 Docker 转发和其他容器将无法连接。通过宿主机端口访问时，容器可能看到桥接网关来源，因此白名单包含桥接网段。宿主机入口仅绑定 IPv4 回环；这不会阻止该代理访问 IPv6 目标。

需要代理的另一个 Compose 项目可以在其**现有**网络配置中添加以下网络；保留客户端原有网络、卷和业务配置，不要整份替换服务：

```yaml
services:
  client: # 替换为实际客户端服务名
    networks:
      - default
      - warp_proxy
networks:
  default: {}
  warp_proxy:
    external: true
    name: warpdeck_proxy
```

先创建 WarpDeck 网络，再重建需要接入的客户端服务。在该网络中将应用的 SOCKS5 服务端设为 `warp:1080`；支持 URI 的客户端可使用 `socks5h://warp:1080`，让代理解析域名。客户端容器里的 `127.0.0.1` 指向客户端自身，不能用它访问宿主机发布端口。如果客户端采用 host 网络，则可使用宿主机的 `127.0.0.1:40000`，无需把 WarpDeck 改成 host 网络。同一共享网络内避免重复使用 `warp` 别名。

从实际客户端验证，例如其中已安装 curl 时执行：

```sh
curl --fail --max-time 10 --socks5-hostname warp:1080 https://1.1.1.1/cdn-cgi/trace
curl -g --fail --max-time 10 --socks5-hostname warp:1080 'https://[2606:4700:4700::1111]/cdn-cgi/trace'
```

需要纯容器入口时，可移除宿主机 `ports` 发布；需要公网入口时，应根据实际来源明确配置绑定地址和白名单。仅发布 TCP 仍不解决 UDP ASSOCIATE 的动态端口接入，参见[接入与 IPv6](#接入与-ipv6)。

## 部署验收

先等首次注册和连接完成，确认 `docker compose ps` 为 healthy、`warpdeck status --json` 的模式和出口符合预期、`config_in_sync=true`。`proxy` 下两个 gateway 状态应为 `disabled`。健康检查和 `reload` 都不是公网连通性证明。

下方在 **Bash** 中执行，适用于宿主机本地 TCP 入口、双栈 WARP 的 `proxy` / `both` 部署。根据实际部署修改 `proxy_address`；选择 `native` 的地址族应单独验证其预期出口，不套用 `warp=on` 断言。`gateway` 模式需从已接入的网关客户端发请求，不使用 SOCKS 示例。

```bash
set -euo pipefail
proxy_address=127.0.0.1:40000
container_id=$(docker compose ps -q warp)
test -n "$container_id"
initial_restarts=$(docker inspect --format '{{.RestartCount}}' "$container_id")
started=$SECONDS
rounds=0
while (( SECONDS - started < 60 )); do
  test "$(docker inspect --format '{{.State.Health.Status}}' "$container_id")" = healthy
  docker exec "$container_id" /usr/local/sbin/healthcheck.sh
  for target in https://1.1.1.1/cdn-cgi/trace 'https://[2606:4700:4700::1111]/cdn-cgi/trace'; do
    trace=$(curl -g -fsS --max-time 8 --socks5-hostname "$proxy_address" "$target")
    printf '%s\n' "$trace" | grep -x 'warp=on' >/dev/null
  done
  rounds=$((rounds + 1))
  printf 'round=%s elapsed=%ss ipv4=ok ipv6=ok\n' "$rounds" "$((SECONDS-started))"
  remaining=$((60-SECONDS+started))
  if (( remaining > 5 )); then sleep 5
  elif (( remaining > 0 )); then sleep "$remaining"
  fi
done
test "$(docker inspect --format '{{.RestartCount}}' "$container_id")" = "$initial_restarts"
test "$(docker inspect --format '{{.State.OOMKilled}}' "$container_id")" = false
printf 'passed=%s rounds elapsed=%ss\n' "$rounds" "$((SECONDS-started))"
```

任何失败都会返回非零并停止；请求耗时可能使总时长略超过 60 秒。`nc`、TCP 端口连接成功或仅有 `healthy` 都不能代替 SOCKS 请求。需要域名访问时还应验证一个实际业务域名，IP 目标测试不覆盖 DNS；需要 UDP 时另外验证 UDP。其他容器使用的部署还需从该客户端运行请求，不能只测试宿主机入口。

升级前后核对注册身份和两份配置（`config.json`、`applied.json`）、其他服务状态以及宿主机默认路由、策略路由、DNS。身份文件只做本地校验或私有备份，不打印内容。比较路由快照时先排序，并识别 Docker 重建导致的 veth 名称变化、动态过期时间等正常差异；不要仅因文本顺序变化判定宿主机网络被修改，也不要忽略实际下一跳或策略变化。

## 运行与构建

仅一个官方 `warp-svc`，三种模式均使用 `tunnel_only`；需要 SOCKS 时运行一个低权限 3proxy 进程。Bash 监督进程接收明确的应用请求并监督进程、维护隧道路由，不扫描 JSON 文件。Go 编写的 `warpdeck` 仅在命令、配置检查和健康检查时短暂执行，不是常驻服务；编译器不进入镜像。

关键进程退出会导致容器退出，由 `restart: unless-stopped` 恢复；健康检查本身不自动重启 unhealthy 容器。日志与资源限制由 Compose 设置。

Dockerfile 使用公开的 `debian:bookworm-slim` 和官方 Go 构建镜像。独立构建阶段编译控制程序并运行单元测试；另一个阶段下载并校验固定版本的 Cloudflare WARP 与 3proxy 软件包，只提取命令行运行文件和版权资料。运行镜像不包含编译器、WARP GUI 或 WebKit，不执行下载包的维护脚本。

```sh
# 可选：自行构建，不使用 GHCR 成品镜像
sh build.sh

# 告诉通用 Compose 使用刚构建的本地镜像
printf '%s\n' 'WARPDECK_IMAGE=warpdeck:local' > .env
docker compose up -d --pull never
```

自行构建需要访问 Docker Hub、Debian 软件源、Cloudflare 软件源与 GitHub。Compose 默认只拉取镜像，不含 `build` 指令；本地构建统一使用 `build.sh`。

当前固定 WARP `2026.8.2110.0`、3proxy `1.0.1`，下载文件均校验 SHA-256。Debian 运行库使用官方仓库安全更新。升级固定软件包时须同步验证版本、校验值及运行兼容性；不要复制实际部署的数据卷到构建目录。

`.dockerignore` 采用允许列表，只发送构建所需源码；`.gitignore` 排除本地配置、数据卷目录、私钥、日志和备份。只以本项目目录作为仓库根目录，示例配置 `config.example.json` 可提交。不要使用 `git add -f` 绕过敏感文件排除。

依赖来源：[Cloudflare 官方软件包](https://pkg.cloudflareclient.com/)、[3proxy 1.0.1](https://github.com/3proxy/3proxy/releases/tag/1.0.1)、[Debian](https://www.debian.org/)、[Go 官方镜像](https://hub.docker.com/_/golang)。

## 日常维护与升级

常用命令均在包含 `compose.yaml` 的项目目录执行：

```sh
# 查看容器、资源使用和业务状态
docker compose ps
docker compose stats --no-stream
docker compose exec warp warpdeck status --json

# 查看有限的监督进程日志
docker compose logs --tail 80 warp

# 停止 / 再次启动（保留身份与配置）
docker compose stop
docker compose start

# 删除容器和项目网络，保留命名数据卷
docker compose down
docker compose up -d
```

Compose 默认限制容器内存为 256 MiB、内存与 Swap 合计为 384 MiB、进程数为 96。`memswap_limit` 不会替宿主机创建 Swap；实际是否可用取决于宿主机。最大 SOCKS 并发默认为 32，资源消耗还取决于官方客户端版本、流量和连接数，不保证固定内存占用。日志每份 2 MiB、最多 2 份。

更新前保留当前镜像，并备份两个数据卷到仓库外的私有位置。`warp-data` 含 WARP 身份；`warpdeck-state` 含请求配置和已应用快照，应同时保留。分享诊断材料前检查账户信息，不要提交卷内容或实际部署的 Compose 覆盖文件。

```sh
# 保留当前镜像，便于回退；记下输出的标签
set -eu
rollback_image="warpdeck:before-update-$(date +%Y%m%d-%H%M%S)"
docker image tag "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q warp)")" "$rollback_image"
printf 'rollback image: %s\n' "$rollback_image"

# 拉取 .env / Compose 当前选择的镜像
# 如果固定了旧版本或 digest，先将 .env 改为计划升级的引用
docker compose config --quiet
docker compose config --images
docker compose pull warp

# 重建会短暂中断本容器承载的连接
docker compose up -d --no-deps --pull never warp
docker compose ps
docker compose exec warp warpdeck status
```

镜像拉取失败时先停止升级，旧容器可继续运行；不要先删除旧容器或镜像。固定 digest 时按[镜像标签说明](#镜像标签与自动发布)先拉取、核对目标版本，再更新 `.env`，保留其他变量。备份还应包含原 `.env`、Compose 文件和数据卷的实际名称或绑定路径；需要一致的数据备份时安排短暂停机。重建后完成[部署验收](#部署验收)，失败时保留诊断材料并恢复旧镜像；数据格式不兼容时还需恢复匹配的配置备份。

只下载了 Compose 文件的用户也能执行上述升级，不需要 `git pull` 或源码构建。需要更新 Compose 本身时单独审查部署文件变化；不要覆盖自己已有的网络配置。自行构建的用户使用 `sh build.sh --pull` 重建，并保持 `.env` 指向本地镜像。

升级保留原数据卷和 Compose 项目名。不要随意改变项目目录名或 `-p` 项目名：Compose 会按项目名寻找数据卷，名称变化可能表现为“身份丢失、重新要求注册”。需要固定名称时，在首次部署就选定 `docker compose -p <项目名> ...` 并持续使用。

若需要回退镜像，且旧版本与现有数据格式兼容，将 `.env` 中的 `WARPDECK_IMAGE` 改为此前记录的 `warpdeck:before-update-<时间>`（保留其他变量），然后执行：

```sh
docker compose config --images
docker compose up -d --no-deps --pull never --force-recreate warp
```

新版本验收通过后，按需要保留回退镜像一段时间，或用 `docker image rm <已核对的旧引用>` 精确清理。先检查 `docker ps -a` 和 `docker image ls --digests`，确认旧镜像没有其他使用者；不加 `--force`。一个镜像可能有多个标签和摘要引用，按实际引用逐一处理。共享层仍被新镜像使用时不会释放，不能把旧镜像显示的总大小当作净释放空间。只有本机自行构建过 WarpDeck 才需要考虑构建缓存，按确认过的缓存 ID 定向清理；不要运行全局 `docker system prune -a`、删除其他项目镜像或清理卷。

不要用 `docker compose down -v` 作为修复或升级步骤，它会删除数据卷。普通 `restart` 也不是应用待确认 JSON 的命令；确认修改仍应执行 `warpdeck reload`。修改 Compose 端口、资源或挂载配置则需要 `docker compose up -d` 重建，与容器内 JSON 的应用是两个独立操作。

## 故障排查

| 现象 | 检查与处理 |
| --- | --- |
| GHCR 拉取 denied / unauthorized | 核对镜像地址和标签；公开包可匿名拉取。Fork 维护者需在包设置中手动设为 Public |
| 拉取提示 no matching manifest | 当前仅构建 Linux amd64，检查宿主机架构和 Compose platform |
| 首次启动显示 unhealthy，注册状态为 `waiting` | 确认接受条款后执行 `warpdeck register --accept-tos`；等待隧道连接，再查看状态 |
| 注册命令失败 | 检查容器到 Cloudflare 的网络；可重试同一命令，已有身份不会被主动删除 |
| `config_in_sync=false` | 先 `config show`、`config validate`；内容符合预期后 `reload`，并检查 `config_error` |
| SOCKS 连接被拒绝 | 确认模式包含代理，核对监听地址/端口、Docker 映射和实际来源白名单；默认允许全部来源 |
| SOCKS TCP 正常但 UDP 失败 | 检查 `socks.udp=true`，以及客户端到返回的容器地址和动态 UDP 端口是否可路由；单个 TCP 映射不能解决 UDP 接入 |
| 网关状态 enabled，但整机出口不变 | 容器只开放转发，不自动安装宿主机路由；检查用户自己配置的接入路径和来源白名单 |
| 域名请求失败，IPv4 目标正常 | 检查目标地址族对应的 egress 策略、隧道或原生路径；按用户需要修改策略或解析偏好后 reload |
| 改了 JSON 端口后宿主机端口不通 | JSON 监听端口和 Compose 的目标端口必须一致；调整 Compose 后重建容器 |
| `reload` 报监听地址/端口冲突 | 检查地址是否属于容器、端口是否已被占用；修改后再次确认应用 |
| 容器启动失败，提示 TUN/权限问题 | 检查宿主机 TUN 支持、设备映射及 NET_ADMIN；使用项目提供的 Compose 权限配置 |
| 网络地址冲突 | 部署前修改 Compose 的通用桥接网段、容器地址以及对应的来源白名单 |
| 重启后仍为旧模式 | 有未确认修改时属于预期行为；重启恢复上次已应用快照，明确 reload 才切换 |
| Docker 状态持续 unhealthy | 使用下面的诊断命令检查连接与健康检查；Docker 健康状态本身不会触发重启 |
| 旧版本间歇 unhealthy，同时 IPv6 请求失败 | `0.0.1` 修复了 `pipefail` 下管道提前退出导致的路由误判；先核对实际镜像 revision，升级后验证真实 IPv6 请求。仍失败时继续检查隧道与路由，不通过关闭健康检查掩盖问题 |

```sh
docker compose exec warp warpdeck status --json
docker compose exec warp warpdeck config validate
docker compose exec warp warp-cli --accept-tos status
docker compose exec warp /usr/local/sbin/healthcheck.sh
docker compose logs --tail 80 warp
```

WarpDeck 会维护官方客户端的排除表，让自己的路由策略控制流量。官方排除表出现 `0.0.0.0/0` 与 `::/0` 不代表业务流量绕过 WARP；实际出口由 JSON、`warpdeck status` 和真实请求确认。不要手动修改这些内部排除项。

`warp-cli` 用于查看官方客户端状态；业务模式、白名单和解析策略统一用 `warpdeck` 管理，避免手工修改官方客户端模式导致运行状态与监督进程预期不一致。健康检查验证本地服务和隧道，不保证任意外部网站可达。

## 项目结构

```text
.
├── .github/workflows/image.yml  # 构建、检查并发布到 GHCR
├── Dockerfile                  # 多阶段构建与运行依赖
├── CHANGELOG.md                # 版本更新记录
├── compose.yaml                # 默认拉取 GHCR；无宿主机接管脚本
├── config.example.json         # 完整默认业务配置：SOCKS、双栈 WARP、允许全部来源
├── build.sh                    # 镜像构建入口
├── cmd/warpdeck/
│   ├── main.go                 # 配置校验、命令、配置渲染与状态
│   └── main_test.go            # 配置解析与渲染测试
├── entrypoint.sh               # 注册等待、隧道和服务监督、明确应用
├── healthcheck.sh              # 容器健康检查
├── common.sh                   # 监督脚本共用函数
├── scripts/fetch-packages.sh   # 固定版本下载和 SHA-256 校验
├── tests/
│   ├── integration.py          # 隔离网络端到端验证
│   ├── route-pipeline.sh       # pipefail/SIGPIPE 路由检测回归测试
│   └── udp_probe.py            # UDP DNS 与 SOCKS5 UDP 探测
├── go.mod
├── .dockerignore               # 构建上下文允许列表
└── .gitignore                  # 排除运行身份、个人配置与构建产物
```

仓库仅包含容器源码、通用示例和测试，不需要任何既有 VPS 的镜像、配置、密钥或注册数据。

## 隔离验证

构建会运行 Go 单元测试。额外的网络回归测试需要 Linux 上已有 Docker、Python 3、`nsenter` 和相应权限：

```sh
docker pull ghcr.io/poohyeban/warpdeck:latest
sudo python3 tests/integration.py --image ghcr.io/poohyeban/warpdeck:latest --accept-tos
```

测试要求宿主机有可用的原生 IPv4 和 IPv6。脚本在独立容器命名空间中分别模拟双栈、IPv4-only、IPv6-only 的隧道承载网络，覆盖四种出站组合、双栈 SOCKS/网关 TCP/UDP、全部来源和拒绝来源、三模式、手动 reload、失败配置、重启身份保持及断线重连。某个原生地址族不可用时，测试预期对应 `native` 请求失败而配置仍成功应用。这里的场景判断仅属于测试工具，不存在于运行容器中。

测试创建专用临时容器、网络、数据卷和独立 WARP 身份，结束后删除自己的资源；不修改宿主机默认路由或已有容器。临时网段为 `172.28.82.0/24` 和 `fd80:82::/64`，先确认无冲突。可用 `--scenario dual`、`--scenario ipv4` 或 `--scenario ipv6` 单独执行一个场景；默认 `all`。在独立测试环境运行，不复用生产注册身份或数据卷。
