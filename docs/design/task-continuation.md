# 中断后继续任务

状态：CLI 和 Web 入口已在源码实现，需更新程序并显式升级到 Schema 21；本地安装和真实钉钉运行验收以交付说明为准。实现与验证见[详细稿](task-continuation-detail.md)。

## 使用

应用重启仍会中断当前 Agent 调用。启动恢复后，找到失败任务，让 AI 核对已有成果并继续原请求：

```bash
memgov runtime task list owner-private --status failed
memgov runtime task resume <任务ID>
```

Web 统一服务的「任务」页选择失败任务，在详情点击「继续任务」。页面说明是否能恢复原会话；提交后显示等待 Agent 接手，执行和交付状态仍分别核对。

继续请求会恢复该任务所属的运行模块；统一服务需处于运行状态才能领取任务。单独的 `memgov ui` 诊断页面提供 CLI 命令，不能点按继续。`runtime resume <运行模块>` 只恢复模块，`runtime task resume <任务ID>` 才会为具体任务发送“继续”。

## 保留什么

- 新任务保存可恢复的 Agent 会话。继续时使用原会话和原工作目录，发送“继续”，保留已完成步骤和已有文件。
- 旧版本未保存原生会话，结合原请求、已接受的上下文和原工作目录检查进度后继续；不能找回未保存的推理和工具输出。
- 同一任务保留旧尝试记录，新增执行尝试，便于核对中断前后发生了什么。未开始执行就失败、工作目录缺失或旧受控目录状态不全的任务，需要按提示重新准备。

继续只处理原请求。原文已撤回、修改、到期，私聊已清空，应用已停用，或权限和上下文变化时，不能沿用失效的会话。外部操作或投递结果未知时先核对，不因继续请求而自动重发。

新任务的原生会话文件是临时执行状态，由本地 Claude Code 保存，不是长期记忆，也不是 SQLite 真相源的替代品。会话文件被清理或 Claude 禁用保存时，无法原生恢复；`task retry` 仍可重新开始任务。

## 首次更新

在项目目录编译后，停服、显式升级数据库，再启动统一服务：

```bash
make install
memgov service stop
memgov init
memgov service start --config ~/.memgov/config.dual.yaml --open
```

路径沿用自己的 `--home` 与 `--config`。数据库升级不会由 Web 重启按钮自动执行。

对应[最佳落地场景](../architecture/best-practice-scenarios.md)的事项连续与交付可核对；本期提供人工继续入口，不承诺从每个工具调用的中断点无缝接续。
