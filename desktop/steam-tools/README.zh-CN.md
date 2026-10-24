# TetherFlow Steam 双网加速实验版

**目标：Steam 并行下载分别走家庭 Wi-Fi 和 iPhone 蜂窝。不是单连接带宽绑定。当前没有 iPhone+Windows 真机速度证据，不能承诺 1200 Mbps。**

此入口独立于旧版 tetherflow-win.exe，不启动旧 PAC、不设置系统代理、不改游戏、不解密 TLS、不需要 Steam 密码。覆盖 Steam 使用本地内容服务器的 HTTP 下载，并提供按 SNI 识别原 CDN 的 HTTPS 原样透传（不解密、不安装根证书）。现代 Steam 若不采用本地内容服务器路径，暂不能加速，程序不会把零请求说成成功。

## 安装前的现实条件

1. iPhone 安装并签名运行 `ios/` 下的 TetherFlow App。无签名 `.app` **不能直接安装**；需要自己的 Apple 开发签名及兼容 iOS 27 的 Xcode。现有本地 Xcode/SDK 版本的限制见 `ios/VALIDATION.md`。
2. Windows 安装苹果官方 Apple Devices，信任并配对 iPhone。Windows 入口内置纯 Go USBmux 客户端，默认不再需要单独安装 `iproxy.exe`。它连接现有苹果 USB 服务，不替换驱动；Apple Devices/USB 服务尚需在实机确认。这里**不提供未经验证来源的驱动或二进制下载**。
3. 已确认的 USB 3 / 10 Gbps 数据线和电脑高速端口，iPhone App 保持前台、不锁屏。
4. Windows 正常连接家庭 Wi-Fi。暂不要同时运行旧 TetherFlow/PAC、其他代理、VPN；先保存它们的设置并自行正常退出，而非让新工具覆盖。
5. TCP 80/443 未被 IIS、其他本地 Web 服务占用；显式 `-HttpOnly` 可只启用80，但 HTTPS 将不经此工具。不要关闭整个防火墙，也不需要给局域网其他设备开放代理。

## 构建 Windows 入口（开发者）

在仓库 `desktop/` 下：

```sh
go test -race ./internal/steamaccel ./internal/steamsetup ./internal/usbmux ./cmd/steam-accelerator
GOOS=windows GOARCH=amd64 go build -trimpath -o steam-tools/tetherflow-steam-accelerator.exe ./cmd/steam-accelerator
```

在 Windows 上构建无需 `GOOS/GOARCH` 前缀。原 `desktop/main.go` 的启动器保持不变。

## 一次集中验收

在 PowerShell 中先查看 `Get-Help .\Start-SteamAccelerator.ps1 -Detailed` 或直接读取脚本。不要为了运行脚本永久降低系统执行策略；若被策略阻止，请按你组织的签名/策略要求运行。

1. iPhone 打开 App，点 Start。每次重启代理 token 会轮换；在脚本隐藏输入中填写，不要发到聊天、截图或日志。
2. **先测 USB 应用链路**（不改 hosts、无需管理员）：

   ```powershell
   .\Start-SteamAccelerator.ps1 -CheckPhoneOnly
   ```

   返回经过内容校验的本地 128 MiB 传输 Mbps。不是蜂窝测速，也不是 USB 协商速度。若只有几百 Mbps，先不要进入“满速”验收；USB 协商、iproxy、设备 App 都需要排查。
3. 管理员 PowerShell：

   ```powershell
   .\Start-SteamAccelerator.ps1 -HomeIP 192.168.1.23
   ```

   将 IP 换成 `--doctor` 显示的**家庭 Wi-Fi 本机地址**，不是路由器，也不是 iPhone 网卡。程序选择唯一 USB 手机、内置转发到 App，先验证手机认证诊断，再监听本机 LAN IPv4:80/443；仅在成功后加入临时 `lancache.steamcontent.com` hosts 条目。只允许本机回环和该本机源 IP，拒绝局域网邻居。
4. 完全退出、重开 Steam 一次，下载一个足够大的游戏。观察两个出口 Bytes 是否都增长；家庭出口发送到对应本机接口，手机出口经 USB CONNECT。JSON 状态为 `tetherflow-status.json`，没有 token 或游戏 URL。
5. 查看 Steam `logs/content_log.txt` 是否出现 local content cache/SteamCache 及对应本机 IP。**只有 IP 配置、只有本地请求、只有两个计数都不是速度提升的充分证据**；还要对比单路基准与任务管理器实际网络。
6. 记录同一游戏/地区：家庭单路、手机单路、双路的稳定区间，排除磁盘解压阶段。1200 Mbps ≈150 MB/s。一次双路下载可中途拔线验证新请求回退到家庭网络；已经开始的半截响应不会拼接不同来源，Steam 必须重试该块。
7. Ctrl+C 正常退出；检查 hosts 中 TetherFlow 标记消失。若主进程被强杀，独立 guardian 等待其退出并清理；若 guardian 和主进程一起被杀或电脑断电，**无法保证立即清理**，必须用日志中的 lease 恢复：

   ```powershell
   .\tetherflow-steam-accelerator.exe --recover-lease 日志中的32位lease
   ```

   不会整文件恢复旧 hosts，不会覆盖别的程序后来写的内容。发现其他工具已配置同一发现域，或标记块被修改，则拒绝覆盖，需人工检查。没有偷偷修改 DNS 服务器或安装开机任务。

## 故障判断（先看日志，不反复盲试）

- **认证预检失败**：未启动/前台已丢失、token 已轮换、Apple USB 服务没就绪。此时不改 hosts。
- **port80 bind 失败**：端口占用或权限/网络地址问题，不会安装 hosts 条目。
- **Steam 没有代理请求**：当前客户端未发现/未采用本地内容路径，或 HTTPS 使用无法识别的 SNI。记录 content_log；不要通过强改 HTTPS 域名、安装根证书或关闭安全软件去“修”。
- **两个出口有数据但不提速**：可能调度权重、CDN、磁盘/CPU、USB/蜂窝瓶颈。默认家庭:手机为1:4；可用 `-HomeWeight/-PhoneWeight` 调整。这不是承诺全场景带宽相加。
- **CDN Host 拒绝**：默认允许 `*.steamcontent.com` 和 `*.steampipe.akamaized.net`；部分 ISP 使用自己的 CDN 域名。只有从 Steam 日志确认合法域名后，显式 `-AllowedHosts 'steamcontent.com,steampipe.akamaized.net,确认的CDN域名'`；此参数替换默认列表。不会自动放开任意域名或内网目标。
- **手机断网/后台**：手机出口失败后短暂冷却，新请求使用家庭出口；不保证已在传输的块无缝续传。

## 安全边界与证据

本机 HTTP 内容入口仅 GET/HEAD，HTTPS 入口仅原样转发获准 SNI 的加密流，严控目标域名、解析结果和端口；不是真正缓存服务器，不写入游戏内容。HTTP 内容不提供 TLS 的保密性，不要用它下载敏感信息。Steam 自身内容验证仍需启用。USB 代理需要 token 且仅绑定手机回环。

自动化测试证明的是模拟/本机环境的协议、分流、内容正确性和错误处理。真正的 Steam 接入、双网提速、iOS 27 USB 和签名安装必须保留为未验证，直到有实机日志与数据。调度器统计 HTTP 响应体及 TLS 透传字节（不是下载后解压体积），不代表手机没有扣热点额度；**不保证运营商计费绕过**。

技术依据：
- https://lancache.net/news/2020/01/14/steam-client-now-supports-lancache/
- https://github.com/lancachenet/monolithic/issues/85
- https://support.apple.com/en-gb/111785
- https://github.com/libimobiledevice/libusbmuxd

### 多设备与已有隧道（高级）

`--list-phones` 只列出 USB 设备，不选择 Wi-Fi 同步设备；多台手机时显式
`-DeviceUDID`（对应 CLI `--device-udid`）。程序不会在断线后偷偷换另一台设备。
默认内置 USB 转发绑定随机本机回环端口，不占用固定8283。若你已有确认可用的
iproxy，可用脚本 `-UseExistingTunnel`；或显式 `-IProxy 'iproxy.exe'` 启动外部工具。
这两种模式会加 `--external-tunnel`，默认期望本机127.0.0.1:8283。不需要为了正常使用
额外安装它。内置 USB 实现的模拟服务测试，不等于已经验证 Apple Devices 的实际兼容性。
