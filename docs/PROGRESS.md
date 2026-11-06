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

## R4 集成验证中
已审查并集成3daf1f2：实际peer公网校验、字面私网预拒绝、协议2 info、共享IP向量。PublicAddress保留于HTTPProtocol.swift以兼容显式host编译文件列表（布局偏差，不改功能）。工作者只记录info/legacy禁用后失败，未证明peer/private两个测试在旧码失败，不能外推。主线程Swift全套/Go向量/跨语言3轮已启动；R8手机RAM token暂停恢复与listener重启交Sonnet独立worktree。R5仍在进行，未轮询。

## R5 集成验证中、R6启动
已检查R5恢复日志脱敏/重试预算/端口持有证明并集成；主线程cmd race三轮及Windows vet正在执行。Windows强杀/ACL/锁重试尚未实际验证，必须CI。R6单路对照模式和HTTP实时字节交Sonnet独立worktree（steam-sonnet-r6-20261106）；与R8手机会话不重叠；R7需等R6以避免acquire冲突。

## R6 集成验证中、R7启动
已审查并集成78246b3，显式三种出口模式、HTTP每次下行write累计、TLS不重复计数。主线程race三轮/Windows vet/跨语言3轮执行中；CLI接线在R11，当前不能说用户已能切换。R7认证过期隔离与generation安全刷新交Sonnet独立worktree（steam-sonnet-r7-20261106），R8仍独立进行。工作者报告旧版desktop根包proxy_darwin.go缺fmt导入，非本次范围，暂不修改，交付不能宣称全仓build通过。

## R8集成、Windows CI缺陷修正
已集成7e9f8b8：前台暂停RAM token/重启序列化，主线程SwiftRelease及跨语言检查进行中。发现真实Windows CI42200835327失败于测试Get-Acl模块装载（非ACL变化），测试改用Windows PowerShell .NET GetAccessControl取完整SDDL，保留前后相等断言，需CI确认。R13 Release/签名脚本验证交Sonnet独立worktree，明确不碰账号或安装SDK。R7仍认证实现中。

## R7集成验证中、R9启动
已核对generation锁/407不冷却/旧generation隔离并集成R7；第二个旧test407改502保留原断言，符合新认证语义。Config初始token仍宽松（兼容既有库调用），CLI R10必须Normalize严格输入；已在途旧认证可能再发一次，非重复新调度。主线程race/Windowsvet/跨语言3轮执行中。R8主线程SwiftRelease41项及跨语言3轮通过。R9独立DNS与公平IP预算交Sonnet独立worktree，R13交付脚本并行；不修改既有Android。

## R13集成验证中、R10启动
已集成4c7ecaa Debug/Release产物检查与用户主动签名脚本；附加TetherFlow.xcconfig为可选Signing.xcconfig的非秘密包装，未配置Team/账号。工作者本机只有Debug构建+验证通过，Release目的地发现失败，不能记通过；主线程19项Python/脚本syntax/跨语言优化版本回归进行中。R10隐藏stdin/移除token临时文件/探测后刷新交Sonnet独立worktree，与R9 core独立。最终Release须CI。

## 已补验R7/R13与Windows CI
R7主线程steamaccel race三轮/Windowsvet及跨语言3轮通过；R13主线程19项Python、bash syntax、debug与优化Swift hostfixture跨语言检查通过，未宣称Release app实机构建。Windows修正提交0a7c7e1的CI42201736869全绿，ACL模块故障修正已被实际Windows执行确认（含R2/R5行为测试）。较新2ac9739的CI42202371491尚在运行，不重复轮询。R9与R10仍依赖任务进行中，下一步等待完成自动通知后集成。

## R10集成验证中、R11启动
已核对并集成5439234：exe隐藏stdin、无token文件、候选探测成功才刷新，主线程cmd race/Windowsvet进行中。当前407分类用既有错误文本，R12须替换为类型化；protocol2预检也由R12接线，暂不宣称版本兼容门槛已落实。R11模式入口/同窗口并发指标交Sonnet独立worktree，与R9核心DNS不重叠；旧安装说明待R15统一更新。

## R9集成验证中
已检查并集成6b4ea04：手机hostname CONNECT不再依赖桌面DNS；家庭解析结果保持公网固定校验，多IPv4分配公平拨号预算（总超时<4秒不能保证尝试4个地址，已记录）。CONNECT fixture由IP改hostname是协议变化，不删除完整性/预算断言。主线程race三轮、Windowsvet、跨语言10轮启动，待结果。R13代码2ac9739的CI42202371491已全绿，含Debug+Release构建及产物检查；最新R10/R9尚不在该证据内。R11进行中，R12须等其完成后接线。

## R9主线程验证完成
steamaccel race三轮通过（54.983秒）、Windowsvet通过；跨语言10轮在debug与优化Swift fixture两种编译、直连与原生USBmux两个模式均通过，原始日志/tmp/tetherflow-r9-integration.log，无忽略失败。R11仍待自动完成通知后再集成；不重复轮询代理、不启动重叠main.go写入任务。

## R11集成验证中、R12启动
已检查并集成3725500：CLI/launcher三模式、历史累计与同窗口并发分离、mode报告文件、原子写。主线程cmd race三轮/Windowsvet进行中。active统计采用任一路>1Mbps窗口，低于阈值字节不计sustained，必须明确不等于全程平均。R12分阶段doctor/可选择serial/协议2与蜂窝CONNECT预检交Sonnet独立worktree；补准许token.go最小类型化407分类，禁止凭错误文本推断。

## R12集成验证中、R14启动
已检查并集成09798ba分阶段doctor/可选择serial/协议2+USB+蜂窝CONNECT/TLS预检；407改类型化，probe-host白名单目前CLI复制核心规则，留独立审查同步风险。工作者提到全包并行负载一次失败未提供原因，主线程需全面回归定位，不能以单包通过忽略。R14 Windows opt-in E2E/真实PowerShell行为与manifest包交Sonnet独立worktree，只在GitHub Windows明确开关下允许真实hosts修改，必须失败也恢复。主线程cmd race三轮/Windowsvet运行中。

## 整合回归通过、可靠性独立审查启动
主线程四包一起race三轮全部通过：usbmux3.968s/steamaccel55.193s/steamsetup6.518s/cmd11.371s，未重现工作者先前未定位并行失败，但不据此断言历史根因已解决。CI42203841177（0cccceb）全绿，最新eefc386 CI进行中。冻结eefc386对530d8ff的生产可靠性diff发起独立Codex xhigh/worktree R1审查（steam-reliability-review-r1-20261106），R14仅Windows测试/打包并行无源代码重叠；最终R2须覆盖新增delta。无真实速度证明。

## 独立审查R1阻断，禁止交付
审查报告P2：refresh未验protocol2、startup Inspect先于journal恢复、verify-app strings|grep-q SIGPIPE漏检；P3首读n>0+EOF误记无响应；另跨语言9EOF/计数不符复现后重跑通过，未定位不得关闭。主线程已开始修refresh复用完整probeStartup、strings输出落临时文件并检查生产者错误、EOF判断计入本次n，测试进行中。startup journal/doctor协调尚未修复。跨语言EOF专项交Sonnet独立worktree（steam-eof-investigation-20261106），保留期望字节且不重试掩盖。R14仍独立Windows测试；最终新R2覆盖全部delta。

## 审查恢复顺序修复验证中
主线程增加steamsetup.Recover仅持锁修复日志不改lease；reclaim在持有正确端口证明后先Recover再Inspect，doctor对journal存在的torn状态给WARN且保持只读，实际恢复仍歧义即拒绝。新增库层repair-only和doctor门槛回归；测试运行中。此前三项小修Go/Python回归通过，但最终R2仍须复核，不关闭EOF阻断。

## R14集成验证中、恢复门槛修正
已集成ba8e8db的WindowsE2E/提取包真实launcher行为脚本，仅WindowsGitHubActions显式开关执行；本机仅编译，未记CI通过。恢复顺序修复首次完整cmd测试发现doctor过宽WARN让无效journal掩盖外来修改，已保留旧断言并改为read-only Recoverable识别合法journal后才降WARN；不重写/删除journal。回归进行中，阻断未解除。R15文档须等最后行为和CI确定再写。
