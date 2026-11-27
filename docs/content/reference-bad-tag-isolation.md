# 服务标签坏行隔离与手工配置拒绝（consul tag bad-line isolation）

## 问题

`routecmd.build` 把每个 `urlprefix-` 标签原样拼成一行 `route add` 文本，`serviceConfig`
汇总各服务文本，`makeConfig` 逆序排序后拼接，`watchBackend` 再把这段文本与 KV 手工配置
拼进同一个缓冲区交给 `route.Parse` 与 `route.NewTable`。这两处都是「一处失败、整体失败」：

- `route.Parse` 在第一条语法错误处返回，`NewTable` 返回 nil；
- `addRoute` 里 `glob.Compile` 失败同样让 `NewTable` 整体返回 nil；
- `watchBackend` 只打一条 `[WARN] line N: ...` 就 `continue`，此后其他实例下线、权重调整
  都停在旧表上；启动时第一张表失败，`first` 通道一直不关闭，进程停在
  `Waiting for first routing table`。

可以直接构造的坏输入有三类：

| # | 标签 | 生成的行 | 失败位置 |
|---|------|----------|----------|
| 1 | `urlprefix-/api weight=abc` | `route add api /api http://.../ weight abc` | `reAdd` 匹配，`parseWeight` 返回 `syntax error: weight value invalid` |
| 2 | 普通标签 `env="prod"` | `... tags "env=\"prod\""` | `reAdd` 的 tags 捕获组是 `[^"]*`，带反斜杠转义的双引号让整行判为语法错误 |
| 3 | `urlprefix-/[a` | `route add api /[a http://.../` | `Parse` 阶段完全合法，`addRoute` 的 `glob.Compile` 才失败，且不带行号 |

还有一处连带：`ParseAliases` 对同一缓冲区报错后，`watchBackend` 仍以 `nil` 调用
`registry.Default.Register`，consul 后端 `be.Register` 会把 `dereg` 中不在列表里的服务
逐个注销，于是所有经 `register=` 注册的别名从 Consul 消失，路由表却没变。

## 改动

### 1. `route.ValidateRouteCommand`：只校验、不落表

`route/validate.go` 新增一个单行校验函数。它复用 `Parse` 的语法判定（把 `Parse` 与
`ParseAliases` 的单行解析抽成 `parseLine`，三处共用同一份逻辑），并对解析出的 `RouteDef`
执行 `addRoute`/`delRoute` 里会让表构建失败的检查：

- `route add`：`Src` 非空、`Dst` 非空、`url.Parse(Dst)` 成功、`hostpath(Src)` 的路径
  `glob.Compile` 成功（第三类输入就靠这一步拦住，只做 `Parse` 挡不住）；
- `route del`：仅在带 service、dst 且不带 tags 时校验目标 URL；
- `route weight`：权重非法在 `NewTable` 里只打日志、不中断，因此校验照此放行。

此外拒绝内嵌换行的命令（生成命令被拼进合并缓冲区后会被拆成多条），这一类是防御性的，
`Parse` 本身不会遇到。

函数不接收 `*bytes.Buffer`，也不返回 `line N`：调用方知道自己校验的是哪一行，由调用方
补充位置信息。校验路径不构造 `Target`，因此不会经 `addTarget` 调用 `counters.With`，
也不会在 opts 同时带 `host` 与 `https` 时创建 `transport.NewTransport`。

### 2. 落点选择：新增校验函数 vs. 临时 Table 跑 addRoute

倾向前者，已在 `route/validate.go` 实现。放弃「在 consul 包里对每行 `Parse`，再对一张
临时 `Table` 执行 `addRoute`」的理由如下：

- **它会留下指标序列，已用探针确认。** `addRoute` → `Route.addTarget` 必然执行
  `counters.histogram/rxCounter/txCounter.With(...)`。对 graphite 这类在 `With` 中把序列
  注册进 registry 的后端，校验一条最终被丢弃的行会往 registry 里塞入
  `fabioapi.api_example_com./api.10_0_1_1_9090.rx/.tx/.p50...`，而这条路由从未进入过表；
  `SetTable` 的 `cleanupStaleMetrics` 只对比新旧真表，永远不会回收这些序列。标签
  `urlprefix-/api weight=abc` 生成的 `route add api /api http://10.0.1.1:9090/ weight abc`
  正好触发这一路径（带 `host=`/`proto=https` 的行还会额外创建 `transport.NewTransport`，
  代价更高但结论相同）。prometheus 后端同样是 `With` 时物化序列，只是本仓库版本里
  指标成年在别处注册，探针在默认 registry 上看不到，因此对照表以 graphite 为证。
- **它还有每行的额外开销。** 每条被丢弃的行都会建一个 `http.Transport`（opts 同时带
  `host` 与 `https` 时）并物化三个带标签的计数器，纯属浪费。
- **反方向的好处并不成立。** 临时 Table 与新函数都要与 `addRoute` 的拒绝条件保持同步，
  并没有因为复用 `addRoute` 就免掉这份维护负担；但新函数可以把「哪些条件会让建表失败」
  集中在一处显式表达（`validateRouteDef` 的注释指明要与 `addRoute`/`delRoute` 同步）。

被放弃的做法在 `urlprefix-/api weight=abc` 上出错：它不但拒绝不了任何东西之外的额外
成本，还会留下一条只存在于指标后端的幽灵序列。

### 3. 逐条校验、非法丢弃，落点在 `serviceConfig`

`registry/consul/service.go` 的 `serviceConfig` 对 `routecmd.build()` 的每一行调用
`route.ValidateRouteCommand`：非法行进入「丢弃集合」，合法行照常进入 `config`。同实例的
其他标签、同服务的其他实例、其他服务都不受影响。

`serviceConfig` 的签名从 `[]string` 改成 `([]string, []droppedCommand)`，`makeConfig` 用
一个 `result` 结构收集，校验产生的 `droppedCommand` 汇总后交给 `logDropped`。
`routecmd.build()` 保持原样，`TestRouteCmd` 的期望逐字不变（本次没有改动
`routecmd.go`）。

**丢弃而不是保留上一次合法命令。** 一个实例的全部路由标签都非法时，它会从表中消失；现状下
它停在旧表里还能收到流量。这是有意的行为变化：保留旧命令在「实例换了端口重新注册且标签
写错」时会把流量送往不再监听的旧端口。这类错误的代价（全部流量打到已关闭的端口、连接被拒）
高于「该实例暂时不接流量」，而且被丢弃的事实有 WARN 日志可查。

### 4. 丢弃集合的日志

被丢弃的命令记录：服务名、节点、`ServiceID`、`ServiceTags` 原文（不是生成后的命令）以及
失败原因。原因直接来自 `ValidateRouteCommand`，因此不含合并缓冲区的 `line N`，也不会把
整段合并文本或其他服务的命令打进日志。

`ServiceMonitor.dropped` 保存上一轮的丢弃集合（`key` 由节点、ServiceID、生成命令与原因
构成，因此同一条坏标签在不同实例上是不同的条目）。`makeConfig` 在汇总完所有服务的
`serviceConfig` 之后调用 `logDropped`：

- 新进入集合的条目打一条 `[WARN]`；
- 离开集合的条目打一条 `[INFO]`，不论是被修正还是实例下线；
- 集合未变化时没有输出。

各服务的 `serviceConfig` 并发执行，但 `logDropped` 在等待所有 goroutine 完成之后才运行，
并且把两份集合按键排序后输出，因此日志内容与顺序和 `ServiceMonitors` 的并发度无关。

验收序列（`ServiceMonitors=4`，测试 `TestMakeConfigDropsBadTagOnlyForItsInstance`）：

1. api 在 n1 的实例带 `weight=abc` → 一条 WARN；
2. 只有无关的 web 实例下线 → 无丢弃相关输出；
3. n2 的第二个 api 实例带同一条坏标签转为健康 → 第二条 WARN；
4. n1 改正标签 → 一条 INFO；
5. n2 注销 → 第二条 INFO。

累计恰好两条 WARN、两条 INFO。

### 5. 手工配置：整体拒绝，但不阻塞服务侧

手工配置由人编写，`listKV` 在每个键内容前插入一行 `# --- 键名`，键与键之间用空行连接。
取舍是**保持整体拒绝**：不逐行容错。理由是逐行丢弃会让一次同时含合法 `del` 和非法行的
修改整体只生效一半——操作者想删的路由仍在，而非法行也被静默跳过，编辑意图被部分执行且
难以核对。整体拒绝则让这次修改完全不生效，并在日志里指出键名与行号。

`watchBackend`（`main.go`）在收到手工配置时：

- `validateManualConfig` 校验每行；非法则丢弃这份新配置，`effectiveManual` 保持上一次
  合法的内容，打一条 WARN（同样的错误文本不重复输出）；
- 合法则替换 `effectiveManual`，若此前处于拒绝状态则打一条 `[INFO]` 解除；
- 每次（服务侧或手工侧）变化都用 `svccfg + "\n" + effectiveManual` 组表，因此手工配置
  非法期间服务侧的下线、权重调整照常生效。

错误文本形如 `invalid manual configuration in key "fabio/config" line 2: ...`，指向 KV
键名与该键内容中的行号；`addRoute` 抛出的通配符错误也走同一条路径（由
`ValidateRouteCommand` 返回，由这里加位置）。

**定位不只靠扫描 `# --- ` 开头的行。** 手工配置里可以写 `# --- disabled` 这样的注释，
所以 `splitManualConfig` 只把「`# --- ` 前缀 **加上 KV 路径**」的行当作键分隔行
（`listKV` 写的是 `# --- /fabio/config/many`，去掉首斜杠 `fabio/...` 后比较）。普通注释即便
以 `# --- ` 开头也不会被误认为分隔行。只有当内容逐字复制了某键的完整分隔行时才会误判，
而那种行本来也不承载任何路由命令。

**行号按修剪前的内容计。** `listKV` 原来对键内容做 `TrimSpace`，本次改为保留原文
（仅 `separator` 路径；`WatchNoRouteHTML` 那条不带分隔符的路径仍 `TrimSpace`，行为不变）。
操作者在 Consul 界面看到的是原文，因此 `route del`/`route add` 前的空行、行尾空白都计入
行号。代价是内容末尾的换行会被一并带进合并缓冲区，这只会多出一个空行，`Parse` 忽略空行。

**别名与实际生效的配置一致。** `watchBackend` 现在先建表、再解析别名、最后 `Register`：
`NewTable` 失败就不 `Register`，绝不因为解析失败而以空列表调用 `Register` 注销所有别名。
拒绝期间 `Register` 收到的别名来自 `svccfg + effectiveManual`，即实际生效的那份配置。

验收序列（`TestWatchBackendIsolatesBadManualConfig`，用假后端与无缓冲通道 + 栅栏）：

| 步 | 输入 | 期望 |
|----|------|------|
| 1 | 服务侧：web 两实例、api 一个（`weight=abc` 已在 consul 层丢弃）+ `urlprefix-/api/v2` | `/web` 两个目标，`/api/v2` 指向 api，`/api` 不存在 |
| 2 | 手工侧：`fabio/config` = `route del web /web http://10.0.0.2:8080/` | `/web` 只剩 10.0.0.1 |
| 3 | 同键追加 `route add x /[x http://10.9.9.9:80/` | 被拒绝，表保持第二步，错误指向 `fabio/config` 第 2 行 |
| 4 | 服务侧：10.0.0.1 转为不健康 | 仍用第二步的手工配置，`/web` 无目标而消失，`/api/v2` 不变 |
| 5 | 手工侧：键改回第二步内容 | 表不变，拒绝状态解除并记录一次；重复送入相同内容不再产生日志 |

## 行为对照表

| 坏输入 | 改前路由表 | 改后路由表 | 改前别名注册 | 改后别名注册 | 改前日志 | 改后日志 |
|--------|-----------|-----------|--------------|--------------|----------|----------|
| `urlprefix-/api weight=abc`（实例唯一标签） | 整表返回 nil，表停在上一份 | 该实例从表中消失，其他实例照常 | `ParseAliases` 失败后以 `nil` 调 `Register`，别名全部注销 | 别名来自生效配置，不受影响 | `[WARN] line N: syntax error: weight value invalid` | `[WARN] consul: Dropped invalid route command ... with tags ["urlprefix-/api weight=abc"]: syntax error: weight value invalid`（仅集合变化时） |
| 普通标签 `env="prod"` | 整表失败，停在旧表 | 该实例生成的行被丢弃（该行本身语法校验失败） | 同上，全部注销 | 不受影响 | `[WARN] line N: syntax error: 'route add' invalid` | `[WARN] ... with tags [... "env=\"prod\"" ...]: syntax error: 'route add' invalid` |
| `urlprefix-/[a` | `Parse` 通过，`NewTable` 返回 nil，停在旧表 | 该行被丢弃（glob 校验），同实例其他标签生效 | 别名照常（`ParseAliases` 不编译 glob），表却没变 | 与生效表一致 | `[WARN] unexpected end of input`（无行号） | `[WARN] ... : route: invalid path "/[a": unexpected end of input` |
| 实例全部路由标签都非法 | 停在旧表，仍收流量 | 从表中消失 | 同上 | 不受影响 | 一条整表 WARN | 每行一条 WARN；集合不变时不重复 |
| 手工配置某键第 2 行非法（含合法 `del`） | 整表失败，服务侧变化也停住 | 手工配置整体不生效，沿用上次合法内容；服务侧变化照常 | 全部注销 | 按生效配置 | `[WARN] line N: ...` | `[WARN] invalid manual configuration in key "fabio/config" line 2: ...`，内容修正后 `[INFO] Manual configuration accepted again` |
| 手工配置里写注释 `# --- disabled` | 行的归属被误判（潜在的定位错误） | 视为内容注解，位置仍指向正确的键与行 | — | — | — | — |
| 坏标签在每次健康变化时重现 | 每次都打一条整表 WARN | 只在丢弃集合变化时输出 | — | — | 反复刷屏 | 进入 WARN、离开 INFO，各一次 |

## 兼容性

- 正常标签生成的命令逐字相同；`TestRouteCmd` 未改动。
- `makeConfig` 的逆序排序与拼接顺序不变（丢弃只发生在进入 `config` 之前）。
- `file`、`static` 后端不经过 consul 包，行为保持原样；`custom` 分支未改动。
- `route.NewTable`、`route.Parse`、`addRoute` 的拒收语义不变；手工配置路径只是多了一层
  预先校验，所以一张表仍可能在「手工配置与最新服务配置组合」时失败，此时行为与改前一致
  （打 WARN、`continue`、不 `Register`）。

## 验证

全部离线：

- `route/validate_test.go`：合法/非法命令清单、校验与 `Parse`+`NewTable` 逐一比对、
  错误不带行号、校验不修改路由表也不创建指标序列。
- `registry/consul/service_test.go`：`api.NewClient` 指向 `httptest` 假 Consul（按步骤切换
  `/v1/catalog/service/<name>` 返回的 JSON），健康检查用 `api.HealthCheck` 构造；覆盖
  五步健康序列、三类坏输入、全坏实例消失、原文标签上报、`ServiceMonitors` 1/2/4/16 输出一致
  （配置文本与丢弃日志文本都逐字一致）。
- `registry/consul/kv_test.go`：`listKV` 保留原文（行号按原文计），无分隔符路径仍裁剪。
- `manual_config_test.go`：多键行号、注释与伪分隔行、无键行的报错格式。
- `watch_backend_test.go`：假 `registry.Backend`，无缓冲通道 + 栅栏发送，逐步断言路由表、
  `Register` 参数与日志；不使用 sleep。
