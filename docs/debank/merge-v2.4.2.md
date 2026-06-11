# iotx upstream v2.4.2 merge 验证报告

- **PR**: #10（`merge/upstream-v2.4.2` → `v2.4.2`），merge commit `1652d2b57`
- **测试镜像**: `blockchain/iotex-x:amd64-1652d2b5`（PR dispatch 构建）
- **日期**: 2026-06-10 ~ 2026-06-11
- **结论**: **PASS**，可出 release `v2.4.2-debank-1`

## 1. Release 更新内容与升级决策

upstream [v2.4.2](https://github.com/iotexproject/iotex-core/releases/tag/v2.4.2)（2026-06-10 发布）是安全+稳定性 patch：

- **#4844 安全加固**：修复安全审计认定的 3 个 HIGH 级 peer 可达攻击向量——任意 peer 可用 `Info=nil` 的 NODE_INFO 消息崩节点、actsync 出站流量洪泛、admin 端口未鉴权 POST `/pause` 可停块——外加一组 peer 输入可达的 panic 路径加固（`envelope.LoadProto` / `ExtractRevertMessage` / `AddLog` 空 topics / endorsement nil proto）。admin mux 改绑 `127.0.0.1`。
- **#4840**：block-sync 与 draft mint 竞争导致 `db.ErrNotExist` panic 崩进程（post-Upernavik 必崩）→ mint goroutine recover、丢弃 draft、进程存活；block-apply 路径（`PutBlock`）保持 fatal 安全网。新增 `iotex_mint_panics_total` 指标。
- **#4839**：配置 `historyIndexPath` 的 api 节点 premint 必报 `KVStore() not supported in *workingSetStoreWithSecondary` → premint 自动关闭。**直接修复我们部署形态的问题**。
- **#4816 / #4817**：收编本 fork 先行开发的 hot producer-keys（收编版加了 token 鉴权，fails closed）与 ioswarm coordinator（默认 `enabled: false`）。
- `iotex-proto` → v0.6.6。

**为什么升级**：3 个 HIGH peer 可达向量直接命中我们的暴露面（writer/api 节点都在公开 p2p 网络里，任何 peer 可发 NODE_INFO 崩我们的节点）；#4840 崩进程、#4839 premint 报错都是我们形态实际会踩的。upstream 标注全节点类型 recommended、api/archive 节点 as soon as possible。

**为什么可以不升级（反方论证）**：无硬分叉、无 genesis/config schema 变化、无新激活高度——不升级不会掉链、不会拒块；新功能（producer-keys 热轮换、ioswarm）我们都不使用。即：没有硬性时间点，纯收益驱动。

**结论**：升级。安全修复收益明确，升级窗口宽松（纯二进制替换）。

## 2. Merge 冲突与影响面

**冲突规模**：34 个 git 冲突 + 1 个 git 看不见的编译级冲突。实际决策点 2 个（见下）。

- **26 个 add/add**：全部是 fork 先行功能被 upstream 收编产生的同源对账（`ioswarm/` ×24、`admin_producer_keys.go`、`snapshotexporter/snapshot.go`）。**全取 upstream 收编版**——差异主体是 gofmt 格式化噪音 + upstream 的改进（producer-keys 端点 +97 行鉴权加固；codec 保留了 fork 的 `ioswarm-json` 名并导出化）。
- **8 个 content**：按 patch 归属处理——保留 fork 的 `ErigonDB()` accessor（canonical state-diff 查询）和 `stateDiffCollector` 字段；producer-keys/coordinator 重叠的 5 个文件取 upstream；`evm.go` 取 upstream（`AccessedSlots()` 提升进 `stateDB` 接口，fork 的 Erigon 包装类型靠嵌入提升自动满足，原类型断言 switch 等价废弃）。
- **1 个编译级**：`StateDBAdapter.AccessedSlots` 两边加在同文件不同位置，git 自动合并无冲突、Go 重复声明编译错；两份实现逐字相同，删 fork 份。

**决策点**（均已裁定）：

1. **ioswarm fork 后续增量弃用**（`40676b838`：32MB gRPC 消息上限、nonce-race shadow 排除、per-agent rewards API——upstream 收编时未带）：与 upstream release 保持一致，弃用。依据：ioswarm 默认关闭、不在 debank 镜像执行路径；保留私有版会成为后续每次 merge 的永久冲突源；git 历史可随时 cherry-pick 找回。**2026-06-11 用户确认。**
2. **fork 私有 dynamicFields 日志机制整套删除**（`SetDynamicFields`/`dynamicFieldsCore` + 测试）：upstream 收编 producer-keys 时改用轮换时显式日志行的方案；grep 全仓该机制仅 ioAddr 注入一处使用，无 debank 功能依赖。

**正向影响（upstream 改动 → pipeline 采集）**：

- `ExtractRevertMessage` 加固是唯一触碰数据面的语义变化。旧代码对「selector 匹配但 ABI malformed」的 revert payload 是**未检查切片直接 panic**；新代码返回 error、action 作废。**Replay 安全**：能触发该路径的块会让所有 v2.4.1 节点崩溃，链上不可能存在——历史 replay 与正常 revert 的 receipt 语义不变。行为变化仅限 simulate 类 API（对 malformed payload 返回 error 而非带 junk-hex 消息的 receipt）。调用方（`erigonstore/contract_backend.go` ×2、`evm.go`）均由 upstream 同步适配。
- `AddLog` 空 topics guard：防御性；pipeline 依赖的 `IN_CONTRACT_TRANSFER` 路径不变。
- `db_bolt.ForEach` / `kvstorewithbuffer`：纯新增。
- mint 路径变化（#4839/#4840）：我们形态 premint 自动关闭，mint 路径整体不再运行；采集 hook 挂 block commit 不挂 draft，无影响。

**反向影响（debank patch → 链正确性）**：

- pipeline hooks 注入点：`statedb.Validate/Mint/PutBlock`（collector，per-call 新建）+ `blockchain.MintNewBlock/commitBlock`（`bc.logger`，无状态函数表）。
- #4840 的 panic-recover 可能让 draft 半途中断 hook 序列（OnTxStart 无对应 OnTxEnd）——collector 随 ctx 丢弃无泄漏，函数表无状态；该路径仅 delegate（出块）形态可达，我们 api/archive 形态 premint 已关，完全不跑。记录为未来 delegate 形态部署的已知边界。
- merge 后 `consensus/` 相对 upstream 净残留 2 行（`TipInfo.StateDigest`，pipeline 消费）。
- `go.mod` 零漂移（`Chaintable/pipeline` pin 与 `go-ethereum-iotex` replace 完好，`go mod tidy` 无 diff）。

## 3. 部署情况

**线上 writer 模式说明（与测试隔离的依据）**：生产 writer pod 为 node + etl（background-tracer）+ jrpcx 三容器，数据投递走 **etl sidecar 调 node 的 `trace_debankBlock`（RPC Tracer 拉取模式）**，node 进程不内嵌 live tracer（卷内 config 无 `VMTraceConfig`），**因此不存在、也不需要 `is_backup` 字段**——隔离方式就是测试部署不跑 etl 容器，node 自身不会向生产 kafka/S3 投递任何数据。

**测试部署**：生产数据卷快照恢复（archive 形态完整数据），单 node 容器，entrypoint/config/genesis 与生产 sts 一致（config/genesis 在数据卷内，`-plugin=gateway`），端口仅绑 127.0.0.1。

**启动前 preflight（卷内 config.yaml 实测）**：`VMTraceConfig` 未配置（无 live tracer）；`masterKey`/`producerPrivKey` 未配置（p2p 身份每次随机、非 delegate，无身份撞车）；`historyIndexPath` 已配置（archive/api 形态，#4839 premint gate 生效）；`httpAdminPort` 0=禁用（admin 改绑与部署无关）。

**compose 文件**（部署实文）：

```yaml
services:
  iotex-writer:
    image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/blockchain/iotex-x:amd64-1652d2b5
    container_name: iotx-writer-merge-v242
    entrypoint: ["iotex-server", "-config-path=/var/data/iotex-archive/etc/config.yaml", "-genesis-path=/var/data/iotex-archive/etc/genesis.yaml", "-plugin=gateway"]
    user: "0:0"
    volumes:
      - /opt/app/iotx/writer_merge_v2.4.2/data:/var/data
    ports:
      - "127.0.0.1:25014:15014"   # http web3
      - "127.0.0.1:24014:14014"   # grpc
      - "127.0.0.1:28080:8080"    # probe/stats
    mem_limit: 10g
    restart: unless-stopped
    stop_grace_period: 60s
    networks:
      - iotx-merge-net
    logging:
      driver: json-file
      options:
        max-size: "100m"
        max-file: "5"
networks:
  iotx-merge-net:
    driver: bridge
    ipam:
      driver: default
      config:
        - subnet: 10.46.89.0/24
```

注：生产 sts 的 command 是 runmode wrapper（downwardAPI 注入，k8s 专用）+ exec iotex-server，本地等价于直接 normal 模式启动，故 entrypoint 仅保留 exec 行；资源（mem 10g）、user（root）、无 env 无 probe 均照抄生产实时 spec。

## 4. 部署后测试情况

**Build & 单测**：

- `go build ./...` 全量、`go vet`、`gofmt`（手工合并文件）全部干净
- `go mod tidy` 零 diff
- 受影响包测试（evm / state/factory / erigonstore / nodeinfo / server/itx / blockchain / rolldpos / db / ioswarm / api）通过
- **baseline 失败**（merge 前 `v2.4.1` 线复跑失败集合一致，非本次引入）：`blockchain/blockdao`（`Test_blockDAO_Stop`、`TestBlockIndexerChecker_CheckIndexer`，flaky 性质）与 `api/TestEstimateExecutionGasConsumption`（测试 mock 的 nil 指针）

**同步验证**：

- 追块：新二进制 blocksync ~7.5 万块（48,991,310 → head），全程 post-Yap（48,985,561）规则；卷 hydration 完成后稳定 10-12 blk/s
- 稳态：与官方 Babel RPC 完全 lockstep——同时刻双端采样 6 次 lag = `1,0,0,0,0,0`，`eth_syncing=false`
- 稳定性：0 容器重启、采样窗口 0 error 行、103 个分钟级样本
- 五项判定（启动日志 / 采样 / head 增长 / not syncing / lag 阈值）：**全部 PASS**

**Hash 抽样 vs `babel-api.mainnet.iotex.io`**（双方均返回 IoTeX native hash，可直接比对）：

| 区间 | 块数 | 结果 |
|---|---|---|
| 49,000,000–49,000,019（追块段） | 20 | 20/20 MATCH |
| 49,066,480–49,066,499（近 head 新块段） | 20 | 20/20 MATCH |

## 5. 过程中暴露的其他问题

1. **release workflow 镜像仓库名不一致**：`release.debank.yml` 原 IMAGE=`blockchain-iotex`，与 PR 构建（`build.debank.yml`）的 `blockchain/iotex-x` 不同仓库。本 PR 已统一为 `blockchain/iotex-x`（commit `9689e6d24`）。**生产升级注意**：现网 sts 仍跑旧仓库 `blockchain-iotex:amd64-v2.4.1-debank-1`，bump 到 v2.4.2-debank-1 时 image repository 必须一并切换。
2. **baseline 测试失败**（见第 4 节）：`blockchain/blockdao` 数个 + `api/TestEstimateExecutionGasConsumption` 在 v2.4.1 工作线上即失败，建议后续单独修复或跟进 upstream。
3. **镜像 ENTRYPOINT 与 compose 语义**：本镜像带 `ENTRYPOINT ["/usr/local/bin/iotex-server"]`，compose 渲染必须用 `entrypoint:` 而非 `command:`（后者是 CMD 不覆盖 ENTRYPOINT，会导致 argv 重复、全部 flags 被丢、config 走默认路径 fatal）。已固化进流程文档。
4. **EBS 快照 lazy restore**（基础设施，非本仓库问题）：1TiB 快照卷未预热时同步仅 0.04 blk/s（"Queue is full" 刷屏），dd 全盘预读 + 临时调高卷 IOPS 后恢复正常。记录给后续大卷链测试参考。
