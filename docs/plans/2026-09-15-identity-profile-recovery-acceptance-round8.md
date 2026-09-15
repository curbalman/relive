# T5 第八轮验收：代码通过，构建交付与完整副本待完成

日期：2026-09-15  
对照：[第七轮验收](2026-09-15-identity-profile-recovery-acceptance-round7.md)、[既定任务](2026-09-15-identity-profile-recovery-rework-round5.md)。

**结论：上轮剩余代码与测试项复验通过；整项交付尚未完成。现有 verify.bin 的源码摘要注入失败，且为 macOS ARM64 产物，需正确构建后再完成 NAS 独立副本验证。**

## 1. 本轮通过的部分

- G1：具有 Package 的执行事件缺 Time 时拒绝；缺退出码、时间不符及包未完成的反例通过。
- G2：新关闭使用实际 `[CollectedAt, RunEndedAt]` 窗口检查；运行前关闭、缺窗口拒绝，窗内关闭及历史 unchanged_resolved 可通过。没有继续使用前后两小时或 time.Now() 兜底。
- FullCLI：普通 go test 下实际执行成功路径，没有 skip；临时 SQLite 夹具触发真实 ANN 重建，Attach 时 ANN 已就绪，完整 loader → 断言 → JSON → exit 0 通过。坏证据路径也通过。
- 默认工具包测试和 service/repository 的 Acceptance/Reacceptance/ThirdReview `-race` 回归通过。
- 当前源码摘要与交付 manifest、358 文件清单完全一致：`ba82b2cea28e07dbd92a0da9fa9b54600889ae34324f965d208fb182a7a83d14`。
- 原始正式测试日志哈希与 manifest 一致；五个必需测试及 service/repository 包均实际 pass。

本轮没有新增针对上述业务修复或断言逻辑的阻塞发现。G3 脚本退出码修复沿用上一轮已验证结果，没有重复进行故障注入。

## 2. 交付构建问题：摘要没有真正注入可执行程序

对象：`/tmp/relive-recovery-20260915-round7/verify.bin`。

其 SHA256 与交付说明一致：`fb686ba271e07d3ab5f4390fad8b749177b8bddc9f778a0cfb70cd329554f67e`，但一致的文件哈希并不证明构建参数生效。

本轮在隔离的临时 SQLite 夹具上实际执行该文件，读取它写出的 JSON，确认：

```text
build_source_digest = ""
test_evidence.digest_match = false
test_evidence.status = blocked
```

`go version -m` 显示构建时使用：

```text
-X github.com/davidhoo/relive/cmd/relive-identity-recovery-verify.buildSourceDigest=ba82b2ce...
```

而 `go tool nm` 显示该独立程序中的实际符号为 `main.buildSourceDigest`。完整包路径曾用于 go test 的测试包注入，不能照搬为这个独立 main 程序的构建参数。

应使用任务原本给出的独立程序参数，在 backend 目录构建：

```sh
go build -ldflags "-X main.buildSourceDigest=${SOURCE_DIGEST}" \
  -o "$ARTIFACT_DIR/verify.bin" ./cmd/relive-identity-recovery-verify
```

SOURCE_DIGEST 必须来自当前实际计算。构建后还需在临时夹具运行并核对 JSON 的 `build_source_digest` 与 evidence 匹配，不能只看编译退出码或 go version 中打印的参数。

本轮工具能正确把这个错误构建判为 blocked，说明失败保护已经起效。

## 3. NAS 产物与完整副本尚未就绪

`go version -m` 显示现有文件为 `GOOS=darwin`、`GOARCH=arm64`、`CGO_ENABLED=1`。它可以用于本地 macOS 验证，不能直接按交付说明 §3.7 的建议复制到 NAS Linux 容器执行。

在适配 NAS 实际架构及 CGO 运行环境的 Linux 构建环境生成产物，使用正确的 main.buildSourceDigest 注入；保存架构、工具链、构建参数、源码和二进制哈希。不要只改扩展名或把 macOS 文件当 Linux 程序。

交付说明本轮已如实标记“完整副本待跑”。完成正确构建后，沿用已校验的原备份建立新工作副本，跑当前工具，补齐新报告、前后状态、退出码和日志。该项仍属于原任务的交付要求；无需重做已通过的 T1–T4 或另行扩大算法范围。

## 4. 本轮实测材料

目录：`/tmp/relive-acceptance-round8-20260915/`。

- `tool-tests.log`、`race.log`：默认工具测试及针对性业务回归通过。
- `targeted.log`：G1/G2 正反例及 FullCLI Success/BadEvidence 实际运行通过，无跳过。
- `source-current.json`：当前 358 文件源码摘要及清单。
- `main_overlay_test.go`、`artifact-overlay.json`、`artifact-probe.log`：真实执行交付二进制，复现其 build_source_digest 为空。临时 overlay 不覆盖正式测试；夹具运行是构建身份核对，不是全库恢复验收。

在 backend 目录复现构建身份问题：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test -v \
  -overlay /tmp/relive-acceptance-round8-20260915/artifact-overlay.json \
  ./cmd/relive-identity-recovery-verify -run '^TestEighthReview_' -count=1
```

本轮只新增验收报告及临时材料，没有修改业务代码、替换交付产物、提交、推送或部署。没有访问 NAS 或运行完整副本；生产未操作。

下一步是**正确构建 → 运行核对摘要 → 完整副本验证 → 回填交付结果**，不再要求重修本轮已通过的代码项。
