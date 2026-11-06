# TetherFlow 当前进度

更新：2026-11-06；分支 wip/steam-ios-dual-download。

## 目标与阶段
Windows Steam 同时利用家庭 Wi-Fi 和 iPhone 蜂窝下载，目标接近手机1200Mbps；不是仅配置代理或显示以太网图标。当前进入第二阶段逻辑复核与剩余任务规划。此前实施约49分钟；本阶段刚启动。

## 已完成与证据
实现 ios/ 前台认证蜂窝出口、desktop/internal/{usbmux,steamaccel,steamsetup}、独立Windows启动器和诊断。代码冻结7a0b54c，文档66dd67b。CI40083910486 Windows/macOS通过，独立审查R2 approve；无PR。详细证据 notes/steam-verification-final.md。

## 尚未完成
真实Apple Devices兼容性、iOS签名安装、Windows+iPhone USB吞吐、Steam实际采用与双路提速未验证。用户目前没有电脑，不能要求立即真机测试。当前方案走USBmux应用隧道，不依赖出现iPhone Ethernet网卡；仍依赖Apple USB服务，未证明修复苹果原生以太网驱动问题。

## 当前委派及下一步
Astra审计已完成，确认不仅差真机：同机重连、重新认证、传输健康、DNS、实时测量、诊断和hosts崩溃恢复均有源码缺口。Claude环境重新检测已恢复。已启动claude-opus-5-5/high独立worktree制定安全契约及剩余执行计划（clientRequestId steam-opus-contracts-20261106），仅允许写计划文档，不改源码；完成自动唤醒，不轮询。接收计划后按依赖逐个分派Sonnet有界实现，主线程集成与验证。不得将旧R2批准外推为这些新契约已满足。

禁止修改原有未提交Android HighSpeedProxyEngine.kt。保留现有实验包，不把CI通过宣称成1200Mbps实机达标。

## 计划已集成
Opus计划docs/STEAM-RELIABILITY-PLAN.md已集成；新分支wip/steam-reliability，CI触发已扩展。开始R1同serial重连，Sonnet独立worktree实施。其余按计划依赖推进，未声称完成。

## R1 集成（2026-11-06）
已检查五文件diff并cherry-pick为6c66b6c。同serial新DeviceID重连，Client.Connect保留严格ID语义；ConnectFailures包含关闭/取消，不应当作健康失败统计。主线程执行usbmux race三轮与Windows vet（结果见线程）。R3传输健康/HTTP停滞超时已交Sonnet独立worktree，clientRequestId steam-sonnet-r3-20261106，完成自动唤醒。R2、R4及后续任务仍未实施。

## R3 集成验证中
已检查并集成4ff204a：HTTP body无进展超时、TLS首因健康分类；旧idle测试改为按新契约验证upload stall冷却，其他取消/keepalive健康断言保留。主线程race三轮、Windows vet及现有跨语言脚本已启动，结果未出前不记通过。工作者称无跨语言对应不准确：仓库integration/verify-host.sh可回归真实Swift+Go传输，主线程补跑。R4 iOS实际peer公网策略与协议2交Sonnet独立worktree，完成自动唤醒。

## R3 验证通过、R2启动
主线程R3检查完成：steamaccel race三轮、Windows vet通过；TF_INTEGRATION_COUNT=3 bash integration/verify-host.sh通过，含直连/原生USBmux模拟服务两个模式的HTTP/TLS字节完整性。R2 hosts事务日志/故障恢复交Sonnet独立worktree（steam-sonnet-r2-20261106），与R4独立，禁止修改cmd层。暂不发布更新实验包。

## R2 集成验证中
已核对日志格式/大小界限与五种恢复状态并集成3997e13，原hosts_test未改。主线程race三轮与Windows vet正在运行；Windows DACL/强制文件锁尚需CI实际执行，不计为已通过。R5 guardian临时锁重试/恢复日志/崩溃清理接线交Sonnet独立worktree，steam-sonnet-r5-20261106。R4仍独立进行，不轮询。
