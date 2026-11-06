# TetherFlow Steam 双网加速实验版

**目标：Steam 并行下载分别走家庭 Wi-Fi 和 iPhone 蜂窝。不是单连接带宽绑定。当前没有 iPhone+Windows 真机速度证据，不能承诺 1200 Mbps，也不声称任何提速。**

此入口独立于旧版 tetherflow-win.exe，不启动旧 PAC、不设置系统代理、不改游戏、不解密 TLS、不需要 Steam 密码。覆盖 Steam 使用本地内容服务器的 HTTP 下载，并提供按 SNI 识别原 CDN 的 HTTPS 原样透传（不解密、不安装根证书）。现代 Steam 若不采用本地内容服务器路径，暂不能加速，程序不会把零请求说成成功。

## 当前状态（2026-11-06，实事求是版）

- 这是可靠性重做后的 `wip/steam-reliability` 分支内容（同 serial 重连、传输健康、token 暂停/刷新、每路 DNS、hosts 事务日志、三种出口模式、分阶段 doctor、Windows E2E 脚本）。**独立代码审查第 1 轮（R1）结论为 block，尚未解除**：其中一项“跨语言传输偶发 EOF/计数不符”只在重跑后通过，历史根因**未知**；本机只在极端并发下复现了 origin 自身 `ENOBUFS`/`EPIPE`，那是证据，**不能证明与当时审查失败是同一原因**。必须由覆盖全部改动的新一轮审查决定，才能交付。
- **最新一次 CI 结果待定**。已记录的 CI 运行：42203841177（提交 0cccceb）全绿；42207422505 在 `TestTLSHomeDNSFailureUsesPhone` 失败，之后的 Windows 健康修复尚无 CI 结论。不要把更早的绿灯外推到当前提交。
- R14 的 Windows 端到端脚本（`Test-Launcher.ps1`、`package.ps1`、Windows 下显式开关的真实 hosts E2E）**尚未在 GitHub Windows 上执行**，本机只编译过。Windows 上“下游客户端已结束后的上游错误按 teardown 分类”的 TLS 健康判断**刚刚修改**，只是启发式（客户端 FIN + 已有下行字节），不是整个响应完整的证明，也可能掩盖客户端半关闭后真实的上游 reset；待 Windows CI 与审查确认。
- **硬件关口 H1–H7 全部未验证**（见 `docs/STEAM-RELIABILITY-PLAN.md` §5）：iOS 27 签名安装、Apple Devices 兼容、蜂窝/DNS、token 手感、Steam 是否采用、三种模式基线、Windows 安全软件与断电恢复。
- 没有声称 1200 Mbps、没有声称 Steam 一定采用本路径、也没有声称修复了苹果原生 iPhone 以太网驱动。USBmux 路径**不需要**出现 iPhone 以太网网卡，但**仍依赖 Apple Devices 提供的 Apple USB 服务栈**。

## 安装前的现实条件

1. iPhone 安装并签名运行 `ios/` 下的 TetherFlow App。无签名 `.app` **不能直接安装**；需要自己的 Apple 开发签名及兼容 iOS 27 的 Xcode（CI 的 Release 构建只是编译和产物检查）。签名由你在自己的 Mac 上运行 `ios/tools/build-signed.sh`（需要 `TF_TEAM_ID`、`TF_BUNDLE_ID`），详见 `ios/README.md`；这是硬件关口 H1，未验证。
2. Windows 安装苹果官方 Apple Devices，信任并配对 iPhone。**不需要 iPhone 以太网适配器/网卡出现**；内置纯 Go USBmux 客户端只连接 Apple USB 服务（USB 栈仍是苹果的），不替换驱动，默认不再需要单独安装 `iproxy.exe`。Apple Devices/USB 服务的实机兼容性尚未确认（H2）。这里**不提供未经验证来源的驱动或二进制下载**。
3. 已确认的 USB 3 / 10 Gbps 数据线和电脑高速端口，iPhone App 保持前台、不锁屏。
4. Windows 正常连接家庭 Wi-Fi。暂不要同时运行旧 TetherFlow/PAC、其他代理、VPN；先保存它们的设置并自行正常退出，而非让新工具覆盖。
5. TCP 80/443 未被 IIS、其他本地 Web 服务占用；显式 `-HttpOnly` 可只启用80，但 HTTPS 将不经此工具。不要关闭整个防火墙，也不需要给局域网其他设备开放代理。

## 构建 Windows 入口（开发者）

在仓库 `desktop/` 下：

```sh
go test -race ./internal/steamaccel ./internal/steamsetup ./internal/usbmux ./cmd/steam-accelerator
GOOS=windows GOARCH=amd64 go build -trimpath -o steam-tools/tetherflow-steam-accelerator.exe ./cmd/steam-accelerator
```

在 Windows 上构建无需 `GOOS/GOARCH` 前缀。原 `desktop/main.go` 的启动器保持不变。`steam-tools/package.ps1` 生成含 MANIFEST（提交号、exe SHA-256、签名状态、“unsigned experimental”）的包；它是 R14 新增，尚未在 Windows CI 实际执行。

## 手机 token：生命周期、输入方式与刷新

**格式与生命周期（iOS 端）**

- 32 个小写 Crockford base32 字符（20 个随机字节），屏幕上每 4 位一组显示；桌面程序会去掉空格和连字符并转小写。
- token 只存在于手机进程内存：不进 Keychain、UserDefaults 或文件。剪贴板复制仍是仅本机、2 分钟过期（通用剪贴板不会到 Windows，所以要手动输入）。
- 锁屏、控制中心、切到别的 App 等任何“非 active”场景只**暂停**：监听口和所有连接都关闭，token 在内存中最多保留 **10 分钟**（单调时钟，睡眠期间也计时）。10 分钟内回到前台，使用**同一个 token** 恢复，无需重新输入。
- **10 分钟暂停重入过期要点**：超过窗口、点 Stop、点 Rotate 或进程被杀，token 即销毁；之后的 Start/Rotate 发新 token，旧 token 一律 407。被挂起的进程在下一次场景事件才能强制执行窗口，窗口内也可能有定时器清除；不要依赖“恰好第 10 分钟”。暂停期间手机路径不可用：`dual` 的新请求走家庭出口，`phone` 模式会失败。这是前台设计，没有后台模式。以上均未在真机验证（H4）。

**桌面输入方式（本次起的实际 CLI）**

- token **只**通过 `tetherflow-steam-accelerator.exe` 自己的隐藏 stdin 提示输入（`iPhone session token (hidden)`）。启动阶段最多 3 次；格式错误、过长（超过 4 KiB 的行被丢弃）、被手机 407 拒绝各算一次；stdin 已关闭（EOF）则直接终止，不启动中继、不改 hosts。
- **不在命令行参数、环境变量、日志或报告里，也不写磁盘**：旧的 `--phone-token-file` 已删除（旧参数现在会被 flag 包直接拒绝），启动器也不再创建临时 token 文件。`-Mode HomeOnly`/`--uplink-mode home` 不打开 USB，也不要 token。
- 错误与恢复日志会把已接受的 token 替换成 `[redacted]`。Go 字符串无法清零，token 在回环和 USB 上是明文，本机管理员可读；这是单用户机器可接受的剩余风险。

**显式刷新（运行中）**

- 手机返回 407 时，手机路径被标为 `Stale`（不是路径错误，**不冷却**）；Stale 期间不再向手机调度，旧 token 不会被反复发送。程序打印 `PHONE TOKEN REJECTED (generation N)` 并再次出现隐藏提示。较旧 generation 的 407 不会把新 token 标为 Stale；已在途的旧认证请求可能再发一次旧 token。
- 新 token 要先通过与启动相同的认证预检（协议 2 的 `/diagnostic/info` 加精确字节 USB 回环）才会替换，替换后清除 Stale 并关闭手机空闲连接；探测失败则保留旧 token 与状态，继续让你重输。
- Stale 期间：`dual` 由家庭出口继续服务；`phone` 模式返回 503，**绝不使用家庭出口**。刷新不改变“手机只走蜂窝”的策略，不会回退到 Wi-Fi。
- 若 stdin 已关闭，刷新被禁用，中继继续运行，需重启程序才能输入新 token。这些交互在真机上的手感（含 32 字符手输、负载下刷新）是 H4，未验证。

## 一次集中验收

在 PowerShell 中先查看 `Get-Help .\Start-SteamAccelerator.ps1 -Detailed` 或直接读取脚本。不要为了运行脚本永久降低系统执行策略；若被策略阻止，请按你组织的签名/策略要求运行。启动器参数：`-HomeIP`、`-Mode Dual|HomeOnly|PhoneOnly`、`-DeviceUDID`、`-CheckPhoneOnly`、`-UseExistingTunnel`、`-IProxy`、`-HttpOnly`、`-HomeWeight`、`-PhoneWeight`、`-AllowedHosts`；`-Mode HomeOnly` 与手机相关参数同时使用会被拒绝。

1. iPhone 打开 App，点 Start，记下屏幕上的 token（暂停 10 分钟内回前台无需换 token）。在程序的隐藏提示中输入；不要发到聊天、截图或日志。
2. **先测 USB 应用链路**（不改 hosts、无需管理员）：

   ```powershell
   .\Start-SteamAccelerator.ps1 -CheckPhoneOnly
   ```

   走与启动相同的 token 提示与认证预检，然后返回经过内容校验的本地 128 MiB 传输 Mbps。不是蜂窝测速，也不是 USB 协商速度。若只有几百 Mbps，先不要进入“满速”验收；USB 协商、iproxy、设备 App 都需要排查。
3. 管理员 PowerShell：

   ```powershell
   .\Start-SteamAccelerator.ps1 -HomeIP 192.168.1.23
   ```

   将 IP 换成 `--doctor` 显示的**家庭 Wi-Fi 本机地址**，不是路由器，也不是 iPhone 网卡。启动器先运行只读 doctor（有 FAIL 即停止），再启动程序：选择唯一 USB 手机、内置转发到 App、依次完成协议 2 / USB 回环 / 手机认证 CONNECT / 证书校验 TLS 探测，**全部在绑定端口和改 hosts 之前**；然后监听本机 LAN IPv4:80/443，仅在成功后加入临时 `lancache.steamcontent.com` hosts 条目。只允许本机回环和该本机源 IP，拒绝局域网邻居。
4. 完全退出、重开 Steam 一次，下载一个足够大的游戏。观察两个出口 Bytes 是否都增长；家庭出口发送到对应本机接口，手机出口经 USB CONNECT。报告文件为 `tetherflow-status-<mode>.json`（每种模式一个文件，互不覆盖），没有 token 或游戏 URL。
5. 查看 Steam `logs/content_log.txt` 是否出现 local content cache/SteamCache 及对应本机 IP。**只有 IP 配置、只有本地请求、只有两个计数都不是速度提升的充分证据**；还要对比单路基线与任务管理器实际网络。
6. 记录同一游戏/地区的三种模式基线（见下节）。1200 Mbps ≈150 MB/s，只是目标参照，不是假定结果。一次双路下载可中途拔线验证新请求回退到家庭网络；已经开始的半截响应不会拼接不同来源，Steam 必须重试该块。
7. Ctrl+C 正常退出；检查 hosts 中 TetherFlow 标记消失。恢复机制见“hosts 日志与手动恢复”。

## 出口模式与基线

| 模式 | CLI | 启动器 | 说明 |
|---|---|---|---|
| 双路（默认） | `--uplink-mode dual` | `-Mode Dual` | 家庭 + 手机，默认权重家庭:手机=1:4（`-HomeWeight/-PhoneWeight`，不保证带宽相加） |
| 仅家庭 | `--uplink-mode home` | `-Mode HomeOnly` | 不用手机、USB、token；用于家庭单路基线 |
| 仅手机 | `--uplink-mode phone` | `-Mode PhoneOnly` | 手机失败或 Stale 时返回 503，**从不**使用家庭出口；用于手机单路基线 |

同一游戏、同一地区分别跑三种模式，比较 `sustained_mbps` 与 `concurrent_seconds`，并排除磁盘/解压阶段；另记录协商 USB 速度和发热。**不预设目标，记录实际发生的事**（H6，未执行）。

### 指标含义（`tetherflow-status-<mode>.json`）

- 每 3 秒一个统计窗口。某一路在该窗口内平均超过 **1 Mbps** 才算“在传输”（过滤 keep-alive 与零星请求）。
- `concurrent_seconds`：**同一个窗口里两路都超过 1 Mbps** 的累计时间；`longest_concurrent_seconds` 为最长连续段。一路早传、另一路晚传不算并发。
- `active_seconds`：至少一路超过 1 Mbps 的窗口累计时间。`sustained_mbps.{home,phone,combined}` = 这些**活跃窗口**内的字节 ÷ `active_seconds`。**它不是整段会话平均**：低于下限的窗口及其字节不计入，空闲时间不拉低它；所以不能与“总字节 ÷ 总时长”比较，更不是速度证明。
- `home_interval_mbps`/`phone_interval_mbps` 是最近窗口的速率；`both_paths_active_now` 表示最近窗口两路是否都活跃；`both_paths_ever` 只是累计字节两路都大于 0 的历史标志，**不等于并发**（旧字段 `both_paths_observed` 已删除）。`phone_auth` 报告 `stale/generation/rejections/stale_since`，不含 token。
- 字节统计：HTTP 为每次向 Steam 连接写入的响应体累计，TLS 透传按转发字节累计且不重复计数；不是下载后解压体积，也不代表手机没扣热点流量，**不保证运营商计费绕过**。

## hosts 事务日志与手动恢复

hosts 修改在 Windows 强制字节范围锁下原地写入，并先写 write-ahead 日志 `<hosts>.tetherflow-<lease>.journal`（与 hosts 同目录，沿用其管理员写权限；仅在操作中或崩溃后存在，不留永久备份）。下次启动/恢复会先处理日志：

- 文件已是新内容（或后面被别人追加了文字）：视为已应用，删除日志；文件仍是旧内容：未应用，删除日志；`new[:k]+old[k:]` 的撕裂写入：补完到新内容后删日志；
- **其他任何情况**说明崩溃后有外来修改：**不覆盖**，保留日志并报 `hosts file needs manual recovery`，错误里写明日志路径。未解决的日志会阻止之后所有 hosts 更新；doctor 会报告“unfinished hosts journal”（日志合法可恢复时为 WARN，否则保持原有失败判定）。处理办法：用管理员权限查看 hosts 与日志，确认没有重要的外来修改后，只手动删除带 TetherFlow 标记的块，核对后再删除日志文件；不要整文件恢复旧版本。
- 残余风险：断电时若扇区乱序落盘，文件不是可识别的撕裂状态，恢复会停下等人工，安全但不自动。
- 主进程退出时 15 秒内清理，超时则由独立 guardian（最多 5 分钟、对临时锁冲突重试）接手；guardian 没有控制台，失败时把 lease 和错误写入 `--recovery-log`（默认 `tetherflow-recovery.log`，在 `--report` 同目录，日志中 token 已脱敏）。
- 陈旧条目只有在其 IP 等于 `--home-ip`、且本进程已持有该地址的端口时才自动接管；否则拒绝，并打印确切的恢复命令：

  ```powershell
  .\tetherflow-steam-accelerator.exe --recover-lease 日志中的32位lease
  ```

  不会整文件恢复旧 hosts，不会覆盖别的程序后来写的内容。发现其他工具已配置同一发现域，或标记块被修改，则拒绝覆盖，需人工检查。不修改 DNS 服务器、不安装开机任务；断电后条目会保留到下次运行或 `--recover-lease`。
- Windows 上的强杀、DACL、锁重试、断电只有 H7 能证明，目前未验证。

## 故障判断（先看日志，不反复盲试）

- **认证预检失败**：未启动/前台已丢失、token 已过期或被轮换（见生命周期）、Apple USB 服务没就绪、手机端仍是不支持协议 2 的旧 App（需更新 iOS App）、手机蜂窝对探测主机不可达。此时不改 hosts。
- **PHONE TOKEN REJECTED**：用 App 当前显示的 token 刷新，见上文；刷新前 `phone` 模式无流量。
- **port80 bind 失败**：端口占用或权限/网络地址问题，不会安装 hosts 条目。
- **Steam 没有代理请求**：当前客户端未发现/未采用本地内容路径，或 HTTPS 使用无法识别的 SNI。记录 content_log；不要通过强改 HTTPS 域名、安装根证书或关闭安全软件去“修”。
- **两个出口有数据但不提速**：可能调度权重、CDN、磁盘/CPU、USB/蜂窝瓶颈。这不是承诺全场景带宽相加。
- **CDN Host 拒绝**：默认允许 `*.steamcontent.com` 和 `*.steampipe.akamaized.net`；部分 ISP 使用自己的 CDN 域名。只有从 Steam 日志确认合法域名后，显式 `-AllowedHosts 'steamcontent.com,steampipe.akamaized.net,确认的CDN域名'`；此参数替换默认列表，并同时约束 `--probe-host`。不会自动放开任意域名或内网目标。
- **目标被手机拒绝（403）**：手机校验实际连上的对端，非公网地址被拒（不冷却，改走另一路）。主机名由手机解析，家庭 DNS 故障不会拦住手机路径；家庭路径仍用 PC 解析，且要求全部答案为公网。
- **手机断网/后台**：手机出口失败后短暂冷却，新请求使用家庭出口（`dual`）；不保证已在传输的块无缝续传。
- **极端高并发的传输异常**：历史审查中出现过一次未定位的 EOF/计数不符；本机只在极端负载下复现 origin 的 `ENOBUFS`/`EPIPE`。这是证据，**不能说明同因，也没有关闭该阻断**。

## 安全边界与证据

本机 HTTP 内容入口仅 GET/HEAD，HTTPS 入口仅原样转发获准 SNI 的加密流，严控目标域名、解析结果和端口；不是真正缓存服务器，不写入游戏内容。HTTP 内容不提供 TLS 的保密性，不要用它下载敏感信息。Steam 自身内容验证仍需启用。USB 代理需要 token 且仅绑定手机回环。

TLS 透传的“健康”分类只看连接生命周期（客户端已 FIN 且已有一些下行字节后，上游的错误按正常收尾处理），不解密、不能证明整个响应完整；Steam 的内容校验仍是完整性依据。

自动化测试证明的是模拟/本机环境的协议、分流、内容正确性和错误处理。真正的 Steam 接入、双网提速、iOS 27 USB 和签名安装必须保留为未验证，直到有实机日志与数据。调度器统计 HTTP 响应体及 TLS 透传字节，**不保证运营商计费绕过**。

技术依据：
- https://lancache.net/news/2020/01/14/steam-client-now-supports-lancache/
- https://github.com/lancachenet/monolithic/issues/85
- https://support.apple.com/en-gb/111785
- https://github.com/libimobiledevice/libusbmuxd

### 多设备与已有隧道（高级）

`--list-phones` 只列出 USB 设备（每行 `serial=… id=… link=…`，不含 Wi-Fi 同步设备），其中 serial 可直接用于
`-DeviceUDID`（对应 CLI `--device-udid`）；serial 只在该命令输出，不写入日志或报告。多台手机时必须显式指定。程序身份按启动时选定的 serial，而非 DeviceID：同一台手机重新插拔得到新 DeviceID 时，下一个连接会重新按 serial 查找并重连；不会在断线后偷偷换另一台设备，也不会走 Wi-Fi 同步。重连、重新枚举行为属于 H2，未在真机验证。
默认内置 USB 转发绑定随机本机回环端口，不占用固定8283。若你已有确认可用的
iproxy，可用脚本 `-UseExistingTunnel`；或显式 `-IProxy 'iproxy.exe'` 启动外部工具。
这两种模式会加 `--external-tunnel`，默认期望本机127.0.0.1:8283。不需要为了正常使用
额外安装它。内置 USB 实现的模拟服务测试，不等于已经验证 Apple Devices 的实际兼容性。

## 体检与启动前探测（`--doctor`，只读）

`--doctor [--uplink-mode home|dual|phone] [--home-ip X] [--device-udid S] [--external-tunnel] [--http-only] [--json]` 分阶段输出 OK/WARN/FAIL：网卡、Apple USB
服务、USB 手机（可选择 serial）、链路元数据（480 Mbps 及以下为 WARN，仅是服务上报的元数据，不是吞吐）、给出 `--home-ip`
时的 80/443 绑定、hosts 检查（遗留 lease 会给出确切的 `--recover-lease <lease>` 命令；未完成的 journal 会提示）。有 FAIL 时退出码为 2；
不写 hosts、不改账户。`home` 模式不需要 USB。doctor 不读 token，所以**不做**协议 2 与 CONNECT 检查；那些在启动和 `--check-phone` 时进行。

手机模式启动时按顺序：`/diagnostic/info` 协议为 2（否则提示更新 iOS App）→ 精确字节 USB 回环 → 经手机认证的 `CONNECT <probe-host>:443` →
用系统根证书验证的 TLS。**协议 2 / USB 回环 / CONNECT 只证明链路可达与协议版本，不是 Mbps，也不是速度。** CONNECT 失败会在绑定端口和写 hosts 之前终止；TLS 失败仅 WARN（探测主机可能不提供 TLS，
可用 `--probe-host` 换一个白名单主机；默认 `steampipe.akamaized.net` 仍待真机确认，H3）。
