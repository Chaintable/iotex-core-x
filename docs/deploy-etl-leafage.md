# IoTeX ETL + Leafage + Consistency Checker 部署计划

## 环境

- **机器**: blockchain-misc-x3
- **Writer**: iotex-archive (`amd64-145771ae`, port 15014, trace_debankBlock 已验证)
- **Chain ID**: 4689
- **Version**: `145771ae` (用于 Kafka topic 命名)
- **参考**: kava 部署 (x1), IoTeX 旧部署 (x5)

## 前置依赖

| 依赖 | 状态 | 说明 |
|------|------|------|
| Writer 节点 | 已就绪 | x3 archive 节点已同步到链顶 |
| etcd 集群 | 已有 | x3 已有 3 节点 etcd (2379/2479/2579) |
| Kafka | 共用集群 | `b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092` |
| S3 buckets | 已有 | inner: `chaintable-nodex-pipeline--apne1-az4--x-s3`, outer: `chaintable-pipeline--apne1-az4--x-s3` |

## Kafka Topics（需手动创建）

从 MEMORY.md：**Kafka topics 必须手动创建 `--partitions 1 --replication-factor 2`，自动创建会变 10 分区导致 leafage 异常**

```bash
# 在能访问 Kafka 的环境执行
kafka-topics.sh --create \
  --bootstrap-server b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092 \
  --topic nodex_pipeline_4689_145771ae \
  --partitions 1 --replication-factor 2

kafka-topics.sh --create \
  --bootstrap-server b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092 \
  --topic pipeline_4689_145771ae \
  --partitions 1 --replication-factor 2
```

## 端口分配

| 服务 | 端口 | 说明 |
|------|------|------|
| Writer (已有) | 15014 | ETH JSON-RPC (trace_debankBlock) |
| Leafage-EVM | **8536** | Read API (与 x5 一致) |
| Consistency Checker | **8731** | 状态 API (与 x5 一致) |

## 服务配置

### 1. ETL (background-tracer)

通过 trace_debankBlock RPC 从 writer 批量拉取区块数据，推送到 Kafka/S3。

```yaml
etl-iotex:
  image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/background-tracer:amd64-v0.1.31
  container_name: etl-iotex
  restart: unless-stopped
  network_mode: "host"
  environment:
    - RUST_LOG=info
    - RUST_BACKTRACE=1
  volumes:
    - /data/iotex-etl-db:/etl-db
  command:
    - /app/background-tracer
    - run
    - --region=ap-northeast-1
    - --nodex-bucket=chaintable-nodex-pipeline--apne1-az4--x-s3
    - --chain-table-bucket=chaintable-pipeline--apne1-az4--x-s3
    - --brokers=b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092
    - --topic=nodex_pipeline_4689_145771ae
    - --rpc-address=http://127.0.0.1:15014
    - --chain-id=4689
    - --genesis-block=0
    - --start-block=1
    - --end-block=0
    - --max-task=32
    - --db-dir=/etl-db
    - --version=145771ae
```

**关键参数说明**：
- `--rpc-address=http://127.0.0.1:15014` — writer 的 ETH RPC 端口
- `--genesis-block=0` — IoTeX genesis block
- `--start-block=1` — 从 block 1 开始（genesis 由 writer 处理）
- `--end-block=0` — 持续运行到链顶
- `--max-task=32` — 并发任务数

### 2. Leafage-EVM (Reader)

从 Kafka 消费数据建立 state 索引。

```yaml
leafage-evm:
  image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/leafage-evm-x:amd64-chaintable-v102-debank-14
  container_name: iotex-leafage-evm
  restart: unless-stopped
  network_mode: "host"
  environment:
    - RUST_LOG=info
  volumes:
    - /data/iotex-nodex:/nodex
  command:
    - /app/leafage-evm
    - standalone
    - --db-path=/nodex
    - --listen-addr=0.0.0.0:8536
    - --chain-cfg=4689
    - --archive
    - --db-cache=8192
    - --meta=127.0.0.1:8536
    - --genesis-number=0
    - --kafka-s3-config={"topic":"nodex_pipeline_4689_145771ae","brokers":"b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092","partition":0,"bucket_name":"chaintable-nodex-pipeline--apne1-az4--x-s3","outer_bucket_name":"chaintable-pipeline--apne1-az4--x-s3","offset_dir":"/nodex/offset","s3_chain_id":"4689","version":"145771ae"}
    - --etcd-config={"endpoints":["127.0.0.1:2379","127.0.0.1:2479","127.0.0.1:2579"],"keep_alive_interval_ms":500,"lease_ttl_s":5}
```

### 3. Consistency Checker

验证 writer 和 leafage 数据一致性。

**配置文件** (`consistency-config.yml`):
```yaml
listen: "0.0.0.0:8731"
ready_ratio: 0.8
check_num: 3
check_interval_ms: 20
chain_id: 4689
version: "145771ae"
consistency_db_path: "/core/consistency_db"
outer_s3_bucket: "chaintable-pipeline--apne1-az4--x-s3"
outer_s3_region: "ap-northeast-1"
inner_brokers:
  - "b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092"
inner_new_block_topic: "nodex_pipeline_4689_145771ae"
inner_new_block_group_id: "consistency-group-4689-145771ae"
outer_brokers:
  - "b-2.chaintablenodexpi.udy5cj.c4.kafka.ap-northeast-1.amazonaws.com:9092"
outer_new_block_topic: "pipeline_4689_145771ae"
etcd_endpoints:
  - "127.0.0.1:2379"
  - "127.0.0.1:2479"
  - "127.0.0.1:2579"
available_nodes_ttl: 5
```

```yaml
consistency-checker:
  image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/consistency-checkerx:amd64-v1.0.18
  container_name: iotex-consistency-checker
  restart: unless-stopped
  network_mode: "host"
  volumes:
    - /data/iotex-consistency:/core
    - ./consistency-config.yml:/etc/consistency-config.yml:ro
  command: ["-config", "/etc/consistency-config.yml"]
```

## 部署顺序

1. **创建 Kafka topics**（partitions=1, replication-factor=2）
2. **创建数据目录**：`/data/iotex-etl-db`, `/data/iotex-nodex`, `/data/iotex-consistency`
3. **写入配置文件**：`consistency-config.yml` 到 `/data/iotex-archive/`
4. **更新 docker-compose.yml**：合并 writer + etl + leafage + consistency 四个服务
5. **启动 ETL** — 等 ETL 开始推数据到 Kafka
6. **启动 Leafage** — 从 Kafka 消费建索引
7. **启动 Consistency Checker** — 验证数据一致性

## 待确认

- [x] ETL `--end-block=47083068`（当前链顶），`--start-block=0`
- [x] Version: `145771ae`（nodectl chain version new 生成）
- [x] Kafka topics 已创建：`nodex_pipeline_4689_145771ae`, `pipeline_4689_145771ae`（partitions=1, replica=2）
- [x] leafage-evm: `amd64-chaintable-v102-debank-14`（最新）
- [x] consistency-checker: `amd64-v1.0.18`（最新）
- [x] Kafka topic 用 nodectl 在本机创建，version=145771ae

## 数据流

```
IoTeX Archive Writer (port 15014, trace_debankBlock)
    ↓ (ETL 通过 RPC 拉取)
background-tracer (ETL)
    ↓ (推送到 Kafka + S3)
Kafka: nodex_pipeline_4689_145771ae
    ↓
Leafage-EVM (port 8536, 建索引)
    ↓
Consistency Checker (port 8731, 验证一致性)
    ↓
Kafka: pipeline_4689_145771ae → S3 outer bucket
```
