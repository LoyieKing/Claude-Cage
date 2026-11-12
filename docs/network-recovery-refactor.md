# 网络、共享与终端恢复重构

## 目标与证据

本次改造处理反向 SSHFS 的双向背压互锁、共享故障传播到整个网络、外层 SSH 失联后不重连，以及内层 SSH 生命周期结束导致终端进程退出。

现场增加管道容量只能解除当轮堵塞，不构成根治。sshfs 3.7.3 的 `cache=no` 是 `dir_cache=no` 的兼容别名，不等于关闭文件页缓存；现场 `ss` 的约 364KB 队列是 Recv-Q，不能据此归因于 TCP Send-Q 或公网吞吐。代码还存在 SFTP 发送错误被忽略、关闭与阻塞发送争用同一锁的问题。

## 不变的安全与数据契约

- 容器保持 rootless、私有命名空间、network=none，不新增宿主 bind mount。
- 宿主 EgressGate 仍是业务外网的唯一放行点；恢复不得解除人工暂停、复用失效网络租约或接受新的出口基准。
- SSH 主机密钥、HostProfile、服务器 InstanceID 固定核验。变化时停止自动恢复，要求明确确认。
- 项目继续由受限 SFTP 处理器访问；恢复核验目录对象身份，不能以同路径存在代替授权。
- 不自动重放 HTTP 修改操作、终端按键和 SFTP 修改请求。结果未知与失败必须区分。
- 不把重挂载宣称为旧句柄、旧 mmap 或旧 cwd 的透明恢复。
- 日志不记录代理凭据、会话票据、SSH 密钥或终端敏感登录输出。

## 目标结构与归属

1. SFTP 会话内核：独立收包、执行、完成和发送；有界队列；首个错误取消整个会话；Close 不等待发送锁；完整处理短写。受限服务的单个响应包写入默认上限为 30 秒，空闲读取不受此计时器影响；它是响应写入上限，不是对每次底层部分写入重新计时的细粒度进展检测。
2. Guest SSH 传输：项目共享直接使用 Go SSH channel，不再经过外部 ssh 的双向 OS pipe；复用既有固定密钥和 known_hosts。
3. ShareManager：每个项目独立授权身份、运行代次和恢复状态；共享故障只关闭该共享。底层挂载目录不可写，阻止卸载后误写。
4. RuntimeSupervisor：保存 running/stopped 运行意图；监督网络 worker；Stop 优先且阻断晚到恢复。默认工作区共享与浏览器转发故障不退出网络 worker。
5. 客机终端服务：Linux 客机 PTY master 和 shell 属于独立客机服务。SSH 附着断开只 detach；宿主和 UI 可按会话 ID、客机代次和输出 offset 接回存活进程。macOS/Windows 原生终端既有路径不被 Linux 改造替换。
6. 外层 RemoteConnectionManager：独立心跳、退避、身份核验和新 ConnectionID；只重建已部署服务连接，不触发安装、升级或环境启动。
7. 热路径：复用帧缓冲、合并帧头与数据发送，减少无用复制；不以提高 MTU、替换 gvisor 或增加无界缓冲代替正确性。

默认 Lima 的 builtin 共享同样存在双向 OS pipe 中转。Unix 宿主改为与 sshfs 自身相同的 socketpair：SSH 子进程 stdin/stdout 指向同一个 open-file description，stdin 非阻塞设置同时作用于 stdout，避免 relay 因输出阻塞而停止消费输入。该改造不改变 SSH 的认证配置或文件权限。原生 Windows 的标准句柄不是 Unix socket，保留其平台传输；新的 Linux 工作台项目共享仍使用固定身份的原生 Go SSH channel。

## 会话与恢复状态

### 共享

`authorized -> mounting -> ready -> degraded -> retiring -> recovering -> ready`，另有 `revoked/blocked/stopped`。

取消先封闭新任务准入、关闭 transport 并结束本轮 sshfs，再核验 Cage 挂载身份并清理。已保存服务器项目可在原授权有效时恢复；本机共享到云端的授权仍随外层连接结束而撤销，不跨连接静默续权。结果未知的写入不得重新发送。

### 网络

期望状态为 `running/stopped`，实际状态分 `starting/ready/degraded/recovering/failed/stopped`。网络 worker 重建生成新网络代次并保持外网关闭，使用现有核验流程恢复放行。共享 EOF 和单条转发失败不是网络 worker 的退出理由。Stop 修改运行意图并取消重试后再停止组件。

### 终端

`starting -> running <-> detached -> stopping -> stopped`；客机服务或容器重启后原进程为 `lost`，不可自动重建 shell。

持久 PTY 只覆盖 Linux 客机和容器。宿主控制服务关闭先标记 detach，再取消 SSH，不向客机发送 Stop；只有环境 Stop（`closeAll`）会结束 shell 进程组。没有已保存的客机会话时，普通连接恢复不安装、不启动终端服务。macOS 与 Windows 原生终端保持原有 PTY，不改成这套客机守护进程。

Create 使用唯一操作 ID；响应丢失后查询，不重复执行启动命令。Attach 使用 shell ID、服务代次与输出 offset。输出保持有界内存环，覆盖时显式 gap；缺失控制序列不保证屏幕完整复原。输入区分 admitted/written/unknown，written 只表示写入 PTY；绝不在断线后重放。Stop 明确且幂等。敏感登录终端不保存输出历史、落盘或回放。

### 外层 SSH

`connected -> reconnecting -> connected(new ConnectionID)`，不可恢复身份或认证错误转 `failed/needs_auth`。每次使用新 ticket/cookie jar；旧 ConnectionID 永久失效。恢复不重放 API、WebSocket 输入或文件写入。连接端口可变化，InstanceID 不可变化。口令不为重连额外持久化。

## 六项实施顺序

| 项 | 改造目标 | 验收门槛 |
|---|---|---|
| 1 | SFTP 内核、Go SSH 项目传输 | 混合读写健康对端持续前进；慢/停止读取可取消；发送错误传播；短写正确 |
| 2 | ShareManager 与挂载身份 | A 失败不影响 B/终端；旧 I/O 明确失败；目录替换拒绝；无底层目录误写 |
| 3 | Supervisor 与故障域 | 共享 EOF 不杀网络；网络 worker 恢复；Stop 不被晚到重连反转；人工暂停不变 |
| 4 | 客机持久 PTY | 内层 SSH/宿主控制服务断线后同一 shell 存活；输入不重复；gap 可见；敏感输出无历史 |
| 5 | 外层重连与 UI | 黑洞检测；固定实例重新认证；旧代次拒绝；修改请求不重发 |
| 6 | 热路径与调用方 cutover | 帧边界保持；有限资源；取消可靠；旧死锁路径不再被生产调用 |

各模块可以并行实施，但共享接口与集成归宿主集成方。模块实施期间不单独运行 build/lint/tests/formatter；合并后统一运行针对性检查。

### 集成接口

- `containerenv.Runtime.Recover(ctx, configPath, log) (restarted, error)` 只恢复已经运行且运行意图有效的容器网络，不启动已停止容器。
- `Server.dialGuestSSH(ctx)` 返回独立、固定身份的 Go SSH client，由调用方关闭；文件、控制与终端不共享无界请求队列。
- `Server.restoreGuestTerminals(ctx)` 在已授权项目重新连接后核对客机终端；没有记录时不反复安装服务。
- `terminalManager.detach()` 用于控制服务退出；明确停止环境仍使用终止会话的路径。
- 新网络 worker 恢复后，宿主丢弃旧服务凭据和检查状态，按新 RunID、Epoch、配置摘要安排完整复检；暂停代次变化或 Stop 会取消复检。

## 迁移与限制

保留原容器完整文件系统、项目授权、账号和密钥，不通过删除容器或重建账号迁移。新终端服务不能接管已经由旧 SSH 进程持有的 PTY；旧终端允许自然结束，新的终端使用新服务。未安装新组件时显示组件更新需求，不静默回退到旧会话实现。

旧 Linux Lima 的生成配置原本关闭 SSH host-key 检查，因此首次迁移需通过它已有的受管置备通道读取客机公钥，并在配置私有目录一次性建立独立 known_hosts。此步骤继承原通道的首次信任条件，不是独立的身份证明；建立记录后，后续原生 Go SSH 连接必须固定该公钥，记录损坏或公钥变化时拒绝覆盖。容器已有安装时固定的 known_hosts，不走首次 Lima 信任路径。生成配置中的 Include/Match/本地执行与代理指令不得参与首次信任迁移。

恢复不会保住已断开的公网 TCP、已经退出的 shell、容器重启前的进程内存或 sshfs 旧文件句柄。编译器遇到共享传输中断可能需要用户重试，不能在后台重新执行构建命令。

客机终端服务保持原 guest 用户的 sudo 权限；不向 shell 添加阻断原有提权规则的 NoNewPrivileges。程序和共享安装目录保持 root 所有，不为终端服务改变整个 `/usr/local/bin` 的所有权。去重记录在当前服务代次内有界保留：256 条近期创建记录及 1024 个退休创建 ID；达到安全记账上限后拒绝新建，不通过丢弃旧 ID 或自动重启现有 shell 继续执行。结束会话的输出最多保留 32 个、合计 2MiB；较早内容可能不可附着。

## 验证清单

- 并行读写、64KiB 数据包、小缓冲、慢读写、半关闭、短写、发送失败、取消中发送。
- WRITE 后 FSTAT/截断/FSYNC/CLOSE 的顺序；目录和符号链接替换；受限路径越界拒绝。
- 恢复时撤销共享、人工暂停、Stop、切换服务器、锁定账户；旧代次不重新发布。
- 内层连接丢失、工作台重启、客机服务重启；会话存活/丢失分别正确呈现。
- 输出环覆盖、输入确认丢失、敏感登录历史禁止；非幂等操作不重放。
- 帧协议边界、最大帧验证、取消释放 transport、长期资源有界。
- 实际程序与真实挂载的 smoke 验证独立记录；自动化测试不代替实机验证。

### 可复核的检查入口

集成完成后，从根模块执行修改包的测试，使所有 replace 指向仓库内的 patched dependencies：

```sh
go test -vet=off -race github.com/pkg/sftp github.com/lima-vm/sshocker/pkg/reversesshfs ./internal/containerenv ./internal/remotehost ./internal/workbench
npm --prefix ui run build
npm --prefix ui test
```

执行时可以缩小 `-run` 到本次修改涉及的行为测试；不把未运行的套件写入验收记录。缺少 guest assets 或平台依赖时记录真实限制，不生成伪造 payload 让检查通过。

| 故障注入 | 必须观测的结果 | 禁止的伪通过 |
|---|---|---|
| SFTP 响应发送中关闭 | Reader/Writer/执行队列均结束，Serve 返回发送或取消错误 | 只关闭 socket，不等待 goroutine/句柄退休 |
| 子进程双向同时超过 64KiB | 两个方向完成且内容一致 | 只增大 pipe、只做单向 echo |
| 项目目录被替换 | 原共享拒绝恢复，授权不能转移到新目录对象 | 按同路径存在自动续权 |
| 共享 A EOF | A 降级，B 与现有终端继续工作 | 整个环境重启后声称 A 自动恢复 |
| 网络 worker 死亡 | 新网络代次、旧租约无效、人工暂停保持 | 复用旧 report 直接打开外网 |
| Stop 与重连并发 | Stop 的期望状态生效，晚到连接被关闭 | Stop 返回后后台再次发布 worker |
| 内层 SSH 中断 | 同一客机 shell ID/代次/PID 存活并可重新附着 | 重新执行命令创建一个看似相同的 shell |
| 工作台重启 | 根据持久元数据核对客机进程，原会话接回或明确 lost | 仅按 PID 存在猜测归属 |
| 外层 SSH 中断 | 同一服务器身份、新 ConnectionID、修改请求只发一次 | 缓存并重发 POST、按键或文件修改 |
| 输出 ring 覆盖 | 明确 gap，敏感终端仍没有历史 | 截取尾部后宣称完整画面恢复 |

真实 SSHFS smoke 使用独立的 `/tmp` 源目录和客机临时挂载点，混合并发读取、写入、fsync、rename，并在宿主校验最终文件内容。只清理该 smoke 创建的确切目录与挂载；不替换正在使用的安装二进制、配置或用户项目挂载。

## 实施与验收记录

六项实现已集成到源码。此处只记录实际执行结果，源码构建不等于已经部署到用户实例或生成发行安装包。

- Unix socketpair 饱和双向传输：`go test -count=1 -timeout=20s third_party/sshocker/pkg/reversesshfs/transport_unix.go third_party/sshocker/pkg/reversesshfs/transport_unix_test.go` 通过。真实子进程在 stdin 非阻塞、双向同时传输各 2MiB 的情况下完成且内容一致。该检查不覆盖整个 SFTP 服务或真实 SSHFS 挂载。
- 临时 Go API smoke 已证明 Close 能中断阻塞 Write、无需对端读取即可退休服务器、发送错误无需等对端 EOF 即返回。临时程序已移除。
- 真实 rootless 容器 SSHFS smoke：使用原实例 SSH 入口创建独立客机 `/tmp` 挂载，以新受限 SFTP 实现服务独立宿主 `/tmp` 源目录。8 路混合 read/write/fsync/rename 在约 0.60s 完成，宿主文件 SHA-256 一致；服务器返回 EOF 后退休，确切临时挂载和源目录均已清理。没有替换用户二进制、配置、项目挂载或现有终端。
- `go test -race -count=1 -timeout=90s ./internal/remotehost` 已通过；固定身份、连接代次、心跳、非重放及关闭同步由真实 SSH 测试服务覆盖。
- 项目共享、固定客机 SSH、并发授权记录、恢复 gate 与桌面代次的针对性 workbench 竞态测试已通过；不将其等同于整个 workbench 套件通过。
- 生产 UI 构建已通过并校验 20 个资源。隔离 API/WS fixture 的真实 Chromium 页面已观察到重连期间保留终端、新 ConnectionID 后刷新并恢复工作台、输出缺口可见提示。该视觉检查不是远程公网断线或原生平台验收。
- 最终针对性竞态验证通过：`go test -race -count=1 -timeout=120s ./internal/containerenv ./internal/remotehost ./internal/workbench -run 'Test(PlanRecovery|StopIntent|Recover|ShareFailure|BlockedShare|ShareCleanup|SavedHostRoot|ShareScripts|Frame|ProcessEndpoint|RestartedNetwork|GuestSSH|PinnedGuestSSH|LinuxGuestSSH|GuestDaemon|GuestShell|GuestInput|BeginDetach|CloseAllStopsMarked|RestoreWithoutRecords|Terminal|ProjectShare|LiveProjectTransport|ProjectSelection|ProjectList|ProjectReconnect|RestoreProject|ProjectActions|ConcurrentProjectRecords|RemovedGrant|RecoveredContainerGate|DesktopReconnect|Heartbeat|ManagerGeneration|ExpiredHTTP|Reconnect|DisconnectAndClose|RecoveryDoesNotReplay|CloseBackground|ClosingController)'`。另执行了受影响的 CLI、账号终端与生命周期消费者测试；真实官方 Claude 账号／macOS 专用 CLI 环境仍按既有条件跳过，未将跳过项记为实机成功。
- Linux 客机终端服务的真实 Python 子进程测试覆盖创建幂等、同一进程断线附着、短命令退出后输出与 EXIT、输出覆盖缺口、敏感输出丢弃、输入确认丢失不重放、关闭和 Stop；不是仅检查接口字段或复制常量。
- 直接本机到云端共享的受限 SFTP 也设置响应写入上限。独立 API smoke 阻塞对端读取且不发送 EOF，约 30 秒后得到 context.DeadlineExceeded，并证明原授权对象已退休、不能附着新流。
- 后端 `go build -o /tmp/cage-refactor-verified ./cmd/cage` 通过。构建时先安装临时 uv 工具完成原有 noVNC 资源置备，没有生成伪造 guest payload 或绕过校验。
- 容器网络 worker 的全流程崩溃恢复、宿主重启／无人登录常驻、macOS／Windows 原生交互未做实机验收。相关生命周期和安全状态由针对性测试覆盖；该限制不被源码构建或浏览器 fixture 替代。
- 检查结束后已关闭隔离浏览器和 UI fixture 服务，移除临时脚本、uv 工具目录与测试二进制；保留源码构建所需的真实前端资源。用户正在运行的安装实例没有切换到新二进制。
- 另在实际 rootless 客机创建独立的临时 systemd PTY 服务：验证普通用户 UID1000、guest sudo UID0，同一个 shell 的 PID／代次在 SSH detach 后保持并可重新附着，明确 Stop 得到确认。只停止该临时 unit 并删除其私有 `/tmp` 文件，没有替换用户的客机服务或安装二进制。
- 拒绝新建的请求不占用新 shell ID、不保留进程、不执行命令；针对安全记账耗尽的资源回归已增加并执行。

### 最新 main 变基验收

重构分支已无冲突 rebase 到目标仓库 main `71046e1af947040709b1868679114253936414d6`。变基后的 SFTP／受限共享竞态测试、容器与外层 SSH 竞态测试、workbench 恢复与持久终端的针对性竞态测试、13 项客户端连接代次测试、生产 UI 20 个资源校验和后端构建均通过。

验证发现原输入确认丢失回归按“第一次连接”注入故障，可能误伤并发启动的输出附着。现按第一条完整 INPUT 请求定向丢弃确认，并要求真实输入在客机文件中只出现一次；没有调整生产逻辑或放宽非重放契约。Go embed 编译需在前端资源构建完成后执行，不能与清空输出目录的 UI 构建并发。
