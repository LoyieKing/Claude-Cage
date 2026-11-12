# Cage 文档入口

首次使用 Cage，从安装与代理配置开始；已有环境遇到问题时，直接查故障恢复。公开安装包与版本验收范围见 [GitHub Releases](https://github.com/Lee-xeo/Claude-Cage/releases)，产品介绍见[项目主页](https://claude-cage.com/)。

## 使用与反馈

面向日常使用的[官网使用文档](https://claude-cage.com/docs/)提供分类目录、全文搜索和章节链接。内容源位于 [guide/](guide/index.md)，包括多账号、浏览器、手机、云端、设置与排障；下表保留安装、平台说明与维护资料的直接入口。

| 你要做什么 | 从这里开始 |
|---|---|
| 让自己的 Agent 下载、安装、配置 Cage，跟踪任务并排查故障 | [Agent 使用指南](guide/agent.md) |
| 选择 macOS、Windows、Linux 安装包，配置代理并打开第一个项目 | [开始使用](getting-started.md) |
| 选择与主机同类的客机系统，了解原生工具、镜像与数据保留 | [选择客机系统](guide/guest-systems.md) |
| 查看快捷键、文件保存、关闭与退出行为 | [日常使用](getting-started.md#日常使用) |
| 在 Windows 上安装、使用和更新 | [Windows 支持](windows.md) |
| 在 Linux 上安装、使用和更新 | [Linux 支持](linux.md) |
| 升级应用、更新环境工具或卸载 | [升级与卸载](getting-started.md#升级与卸载) |
| 查看首个内测版本的能力与验收范围 | [0.1.0-alpha.1 发行说明](releases/0.1.0-alpha.1.md) |
| 排查下载、启动、联网与授权问题 | [常见问题与恢复](troubleshooting.md) |
| 导出故障日志并了解自动清理 | [导出诊断包](troubleshooting.md#导出诊断包) |
| 报告隔离或凭据安全问题 | [安全报告](../SECURITY.md) |

## 实现与恢复契约

[网络、共享与终端恢复重构](network-recovery-refactor.md)记录传输背压、组件故障隔离、持久终端、自动重连的接口、安全边界和验收条件。[未发行源码说明](releases/unreleased-network-recovery.md)列出实现、实际检查和恢复限制，不替代发行版能力说明。

许可材料见 [LICENSE](../LICENSE)、[NOTICE](../NOTICE)和[第三方许可](../THIRD_PARTY_LICENSES)。
