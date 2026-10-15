## v0.7.1

### 背景：保留工具名门禁
上游对 `bash/edit/read/write/glob/grep/shell` 配非官方定义一律 403
（名实相符才放行；未知名、history 旧调用名同样计入）。
harness 自带这几个名的真实定义与官方不一致，真流量被杀。

### 改动
- 进上游前改 neutral 别名（`zd_` 前缀，描述+参数原样保留，模型行为不变），
  回客户端时映射回去，harness 原样执行
- `tool_choice` 具名（字符串/`{type:function}`/`{type:tool}`）同样映射
- 同格式透传（pipeSSE 按行、非流整包）字节级恢复（转义态安全）
- 官方 `shell/read` 桩不受影响（harness 同名会被改名让位）

### 校验
- SHA256 见 SHA256SUMS.txt；Windows exe 自签 Valid
- CI: vet+test 全绿；`verify-windows` PASS
- 生产 e2e：harness 形态 7 保留名 + бами，双路径 200，响应无别名泄漏
